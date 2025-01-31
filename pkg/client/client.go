package client

import (
	"fmt"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"io"
	"ktunnel/pkg/common"
	pb "ktunnel/tunnel_pb"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	bufferSize = 1024
)

type Message struct {
	c *net.Conn
	d *[]byte
}

func ReceiveData(st *pb.Tunnel_InitTunnelClient, closeStream chan<-bool, requestsOut chan<- *common.Request, port int32, scheme string) {
	stream := *st
	for {
		m, err := stream.Recv()
		if err != nil {
			log.Warn("error reading from stream: %v", err)
			closeStream <- true
			return
		}
		log.Debugf("%s; got new request from server", m.RequestId)
		requestId, err := uuid.Parse(m.RequestId)
		if err != nil {
			log.Errorf("%s; failed parsing request uuid from stream, skipping", m.RequestId)
		}
		request, exists := common.GetRequest(&requestId)
		if exists == false {
			if m.ShouldClose != true {
				log.Infof("%s; new request; connecting to port %d", m.RequestId, port)
				// new request
				conn ,err := net.Dial(strings.ToLower(scheme), fmt.Sprintf("localhost:%d", port))
				if err != nil {
					log.Errorf("failed connecting to localhost on port %d scheme %s", port, scheme)
					continue
				}
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				request = common.NewRequestFromStream(&requestId, &conn)
			} else {
				request = common.NewRequestFromStream(&requestId, nil)
				request.Open = false
			}
		}

		if request.Open == false {
			if request.Conn != nil {
				c := *request.Conn
				_ = c.Close()
				ok, err := common.CloseRequest(request.Id)
				if ok != true {
					log.Printf("%s; failed closing request: %v", request.Id.String(), err)
				}
			}
		} else {
			c := *request.Conn
			request.Lock.Lock()
			_, err := c.Write(m.GetData())
			if err != nil {
				log.Printf("%s; failed writing to socket, closing request", request.Id.String())
				ok, err := common.CloseRequest(requestId)
				if ok != true {
					log.Printf("%s; failed closing request: %v", request.Id.String(), err)
				}
			} else {
				go ReadResp(request, requestsOut)
			}
			request.Lock.Unlock()
		}
	}
}

func ReadResp(request *common.Request, requestsOut chan<- *common.Request) {
	conn := *request.Conn
	for {
		buff := make([]byte, bufferSize)
		br, err := conn.Read(buff)
		if err != nil {
			if err != io.EOF {
				log.Errorf("%s; failed reading from socket, exiting: %v", request.Id.String(), err)
			}
			break
		}
		request.Lock.Lock()
		_, err = request.Buf.Write(buff[:br])
		request.Lock.Unlock()
		if err != nil {
			log.Errorf("%s; failed writing to request buffer: %v", request.Id, err)
			_, _ = common.CloseRequest(request.Id)
			break
		}
		requestsOut <- request
	}
}

func SendData(requests <-chan *common.Request, stream *pb.Tunnel_InitTunnelClient) {
	for {
		request := <-requests
		request.Lock.Lock()
		if request.Buf.Len() > 0 {
			st := *stream
			resp := &pb.SocketDataRequest{
				RequestId:            request.Id.String(),
				Data:                 request.Buf.Bytes(),
				ShouldClose:          false,
			}
			if request.Open == false {
				resp.ShouldClose = true
				ok, err := common.CloseRequest(request.Id)
				if ok != true {
					log.Println(err)
				}
			}
			err := st.Send(resp)
			if err != nil {
				log.Errorf("failed sending message to tunnel stream, exiting", err)
				return
			}
		}
		request.Buf.Reset()
		request.Lock.Unlock()
	}
}

func RunClient(host *string, port *int, scheme string, tls *bool, caFile, serverHostOverride *string, tunnels []string, stopChan <-chan bool) error {
	wg := sync.WaitGroup{}
	closeStreams := make([]chan bool, len(tunnels))
	go func() {
		<-stopChan
		for _, c := range closeStreams {
			c <- true
		}
	}()
	var opts []grpc.DialOption
	if *tls {
		creds, err := credentials.NewClientTLSFromFile(*caFile, *serverHostOverride)
		if err != nil {
			log.Fatalf("Failed to create TLS credentials %v", err)
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}
	conn, err := grpc.Dial(fmt.Sprintf("%s:%d", *host, *port), opts...)
	if err != nil {
		log.Fatalf("fail to dial: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()
	client := pb.NewTunnelClient(conn)
	for _, rawTunnelData := range tunnels {
		tunnelData, err := common.ParsePorts(rawTunnelData)
		if err != nil {
			log.Error(err)
		}
		wg.Add(1)
		c := make(chan bool, 1)
		go func(closeStream chan bool) {
			log.Println(fmt.Sprintf("starting %s tunnel from source %d to target %d", scheme, tunnelData.Source, tunnelData.Target))
			ctx := context.Background()
			tunnelScheme, ok := pb.TunnelScheme_value[scheme]
			if ok != false {
				log.Fatalf("unsupported connection scheme %s", scheme)
			}
			req := &pb.SocketDataRequest{
				Port:                 tunnelData.Source,
				LogLevel:             0,
				Scheme:               pb.TunnelScheme(tunnelScheme),
			}
			stream, err := client.InitTunnel(ctx)
			if err != nil {
				log.Errorf("Error sending init tunnel request: %v", err)
			} else {
				err := stream.Send(req)
				if err != nil {
					log.Errorf("Failed to send initial tunnel request to server")
				} else {
					requests := make(chan *common.Request)
					//closeStream := make(chan bool, 1)
					go ReceiveData(&stream, closeStream, requests, tunnelData.Target, scheme)
					go SendData(requests, &stream)
					<- closeStream
					_ = stream.CloseSend()
				}
			}
			wg.Done()
		}(c)
		closeStreams = append(closeStreams, c)
	}
	wg.Wait()
	return nil
}