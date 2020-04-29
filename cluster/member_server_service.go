package cluster

import (
	"context"
	"fmt"
	"github.com/lonng/nano/cluster/clusterpb"
	"github.com/lonng/nano/internal/message"
	"github.com/lonng/nano/scheduler"
	"github.com/lonng/nano/session"
	"google.golang.org/grpc"
	"log"
	"net"
)

type MemberServerService struct {
	node   *Node
	server *grpc.Server //rpc服务端，不应该包含在 cluster里面吗
}

func NewMemberService(node *Node) *MemberServerService {
	m := &MemberServerService{
		node:   node,
		server: grpc.NewServer(),
	}
	clusterpb.RegisterMemberServer(m.server, m)
	return m
}

func (c *MemberServerService) Serve(serviceAddr string) error {
	listener, err := net.Listen("tcp", serviceAddr)
	if err != nil {
		return err
	}

	go func() {
		//rpc server start
		err := c.server.Serve(listener)
		if err != nil {
			log.Fatalf("Start current node failed: %v", err)
		}
	}()

	return nil
}

func (c *MemberServerService) findOrCreateSession(sid int64, gateAddr string) (*session.Session, error) {
	s, found := c.node.sessionService.findSession(sid)

	if !found {
		conns, err := c.node.rpcConnPool.getConnPool(gateAddr)
		if err != nil {
			return nil, err
		}

		ac := &acceptor{
			sid:        sid,
			gateClient: clusterpb.NewMemberClient(conns.Get()),
			rpcHandler: c.node.handler.remoteProcess,
			gateAddr:   gateAddr,
		}

		s = session.New(ac)
		ac.session = s

		c.node.sessionService.storeSession(s)
	}

	return s, nil
}

func (c *MemberServerService) HandleRequest(_ context.Context, req *clusterpb.RequestMessage) (*clusterpb.MemberHandleResponse, error) {
	handler, found := c.node.handler.localHandlers[req.Route]
	if !found {
		return nil, fmt.Errorf("service not found in current node: %v", req.Route)
	}
	s, err := c.findOrCreateSession(req.SessionId, req.GateAddr)
	if err != nil {
		return nil, err
	}
	msg := &message.Message{
		Type:  message.Request,
		ID:    req.Id,
		Route: req.Route,
		Data:  req.Data,
	}
	c.node.handler.localProcess(handler, req.Id, s, msg)
	return &clusterpb.MemberHandleResponse{}, nil
}

func (c *MemberServerService) HandleNotify(_ context.Context, req *clusterpb.NotifyMessage) (*clusterpb.MemberHandleResponse, error) {
	handler, found := c.node.handler.localHandlers[req.Route]
	if !found {
		return nil, fmt.Errorf("service not found in current node: %v", req.Route)
	}
	s, err := c.findOrCreateSession(req.SessionId, req.GateAddr)
	if err != nil {
		return nil, err
	}
	msg := &message.Message{
		Type:  message.Notify,
		Route: req.Route,
		Data:  req.Data,
	}
	c.node.handler.localProcess(handler, 0, s, msg)
	return &clusterpb.MemberHandleResponse{}, nil
}

func (c *MemberServerService) HandlePush(_ context.Context, req *clusterpb.PushMessage) (*clusterpb.MemberHandleResponse, error) {
	s, found := c.node.sessionService.findSession(req.SessionId)
	if !found {
		return &clusterpb.MemberHandleResponse{}, fmt.Errorf("session not found: %v", req.SessionId)
	}
	return &clusterpb.MemberHandleResponse{}, s.Push(req.Route, req.Data)
}

func (c *MemberServerService) HandleResponse(_ context.Context, req *clusterpb.ResponseMessage) (*clusterpb.MemberHandleResponse, error) {
	s, found := c.node.sessionService.findSession(req.SessionId)
	if !found {
		return &clusterpb.MemberHandleResponse{}, fmt.Errorf("session not found: %v", req.SessionId)
	}
	return &clusterpb.MemberHandleResponse{}, s.ResponseMID(req.Id, req.Data)
}

func (c *MemberServerService) NewMember(_ context.Context, req *clusterpb.NewMemberRequest) (*clusterpb.NewMemberResponse, error) {
	c.node.handler.addRemoteService(req.MemberInfo)
	c.node.cluster.addMember(req.MemberInfo)
	return &clusterpb.NewMemberResponse{}, nil
}

func (c *MemberServerService) DelMember(_ context.Context, req *clusterpb.DelMemberRequest) (*clusterpb.DelMemberResponse, error) {
	c.node.handler.delMember(req.ServiceAddr)
	c.node.cluster.delMember(req.ServiceAddr)
	return &clusterpb.DelMemberResponse{}, nil
}

// SessionClosed implements the MemberServer interface
func (c *MemberServerService) SessionClosed(_ context.Context, req *clusterpb.SessionClosedRequest) (*clusterpb.SessionClosedResponse, error) {
	s, found := c.node.sessionService.findSession(req.SessionId)
	if found {
		c.node.sessionService.remove(req.SessionId)
		//延迟关
		scheduler.PushTask(func() { session.Lifetime.Close(s) })
	}
	return &clusterpb.SessionClosedResponse{}, nil
}

// CloseSession implements the MemberServer interface
func (c *MemberServerService) CloseSession(_ context.Context, req *clusterpb.CloseSessionRequest) (*clusterpb.CloseSessionResponse, error) {
	s, found := c.node.sessionService.findSession(req.SessionId)
	if found {
		c.node.sessionService.remove(req.SessionId)
		//直接关
		s.Close()
	}
	return &clusterpb.CloseSessionResponse{}, nil
}
