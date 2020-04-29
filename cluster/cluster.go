// Copyright (c) nano Authors. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package cluster

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lonng/nano/cluster/clusterpb"
	"github.com/lonng/nano/internal/log"
)

// cluster represents a nano cluster, which contains a bunch of nano nodes
// and each of them provide a group of different services. All services requests
// from client will send to gate firstly and be forwarded to appropriate node.
// TODO MasterServer
type cluster struct {
	sync.RWMutex
	// If cluster is not large enough, use slice is OK
	currentNode *Node
	members     []*Member
}

func newCluster(currentNode *Node) *cluster {
	return &cluster{
		currentNode: currentNode,
	}
}

func (r *cluster) Init() error {

	if r.currentNode.IsMaster {
		clusterpb.RegisterMasterServer(r.currentNode.memberService.server, r)
		member := &Member{
			isMaster: true,
			memberInfo: &clusterpb.MemberInfo{
				Label:       r.currentNode.Label,
				ServiceAddr: r.currentNode.ServiceAddr,
				Services:    r.currentNode.handler.LocalService(),
			},
		}
		r.members = append(r.members, member)
	} else {
		pool, err := r.currentNode.rpcConnPool.getConnPool(r.currentNode.AdvertiseAddr)
		if err != nil {
			return err
		}

		client := clusterpb.NewMasterClient(pool.Get())
		request := &clusterpb.RegisterRequest{
			MemberInfo: &clusterpb.MemberInfo{
				Label:       r.currentNode.Label,
				ServiceAddr: r.currentNode.ServiceAddr,
				Services:    r.currentNode.handler.LocalService(),
			},
		}
		for {
			resp, err := client.Register(context.Background(), request)
			if err == nil {
				r.currentNode.handler.initRemoteService(resp.Members)
				r.currentNode.cluster.initMembers(resp.Members)
				break
			}
			log.Println("Register current node to cluster failed", err, "and will retry in", r.currentNode.RetryInterval.String())
			time.Sleep(r.currentNode.RetryInterval)
		}

	}
	return nil
}

// Register implements the MasterServer gRPC service
func (r *cluster) Register(_ context.Context, req *clusterpb.RegisterRequest) (*clusterpb.RegisterResponse, error) {
	if req.MemberInfo == nil {
		return nil, ErrInvalidRegisterReq
	}

	//目前的逻辑是，必需有一个node为master，当有新结点发送register时，该master结点会通知其他结点更新服务
	//master结点承担了消息广播的作用。不过也有单点问题。

	//几种方式：
	//1、配表方式：
	//每个结点启动时本地有一张配表获取其他结点的地址，当结点启动时，会主动连接配表里的结点，并进行服务登记，如果是新结点并更新本地配表

	//2、依赖etcd
	//所有结点在配表里已配置好etcd地址，当启动时会主动发送自己的信息到etcd,所有结点订阅该事件，并进行更新

	//3、nats分布式消息队列
	//所有结点进行通信时，消息都会发送到nats消息队列，各结点也会订阅相关的事件进行消息收发处理

	resp := &clusterpb.RegisterResponse{}
	for _, m := range r.members {
		if m.memberInfo.ServiceAddr == req.MemberInfo.ServiceAddr {
			return nil, fmt.Errorf("address %s has registered", req.MemberInfo.ServiceAddr)
		}
	}

	// Notify registered node to update remote services
	newMember := &clusterpb.NewMemberRequest{MemberInfo: req.MemberInfo}
	for _, m := range r.members {
		//append
		resp.Members = append(resp.Members, m.memberInfo)
		if m.isMaster {
			continue
		}
		pool, err := r.currentNode.rpcConnPool.getConnPool(m.memberInfo.ServiceAddr)
		if err != nil {
			return nil, err
		}
		//收到注册信息，通信所有成员。。。
		//这里应该可以不用每次new client，每次从对象池拿就可以了
		client := clusterpb.NewMemberClient(pool.Get())
		_, err = client.NewMember(context.Background(), newMember)
		if err != nil {
			return nil, err
		}
	}

	log.Println("New peer register to cluster", req.MemberInfo.ServiceAddr)

	// Register services to current node
	//远程服务信息添加至本机remote service
	r.currentNode.handler.addRemoteService(req.MemberInfo)
	r.Lock()
	r.members = append(r.members, &Member{isMaster: false, memberInfo: req.MemberInfo})
	r.Unlock()
	return resp, nil
}

// Register implements the MasterServer gRPC service
func (r *cluster) Unregister(_ context.Context, req *clusterpb.UnregisterRequest) (*clusterpb.UnregisterResponse, error) {
	if req.ServiceAddr == "" {
		return nil, ErrInvalidRegisterReq
	}

	var index = -1
	resp := &clusterpb.UnregisterResponse{}
	for i, m := range r.members {
		if m.memberInfo.ServiceAddr == req.ServiceAddr {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, fmt.Errorf("address %s has  notregistered", req.ServiceAddr)
	}

	// Notify registered node to update remote services
	delMember := &clusterpb.DelMemberRequest{ServiceAddr: req.ServiceAddr}
	for _, m := range r.members {
		//过滤自己
		if m.MemberInfo().ServiceAddr == r.currentNode.ServiceAddr {
			continue
		}
		pool, err := r.currentNode.rpcConnPool.getConnPool(m.memberInfo.ServiceAddr)
		if err != nil {
			return nil, err
		}
		client := clusterpb.NewMemberClient(pool.Get())
		_, err = client.DelMember(context.Background(), delMember)
		if err != nil {
			return nil, err
		}
	}

	log.Println("Exists peer unregister to cluster", req.ServiceAddr)

	// Register services to current node
	r.currentNode.handler.delMember(req.ServiceAddr)
	r.Lock()
	defer r.Unlock()

	if index == len(r.members)-1 {
		r.members = r.members[:index]
	} else {
		r.members = append(r.members[:index], r.members[index+1:]...)
	}
	return resp, nil
}

func (r *cluster) remoteAddrs() []string {
	r.RLock()
	defer r.RUnlock()

	var addrs []string
	for _, m := range r.members {
		addrs = append(addrs, m.memberInfo.ServiceAddr)
	}
	return addrs
}

func (r *cluster) initMembers(members []*clusterpb.MemberInfo) {
	r.Lock()
	defer r.Unlock()
	for _, info := range members {
		r.members = append(r.members, &Member{
			memberInfo: info,
		})
	}
}

func (r *cluster) addMember(info *clusterpb.MemberInfo) {
	r.Lock()
	defer r.Unlock()
	var found bool
	for _, member := range r.members {
		if member.memberInfo.ServiceAddr == info.ServiceAddr {
			member.memberInfo = info
			found = true
			break
		}
	}
	if !found {
		r.members = append(r.members, &Member{
			memberInfo: info,
		})
	}
}

func (r *cluster) delMember(addr string) {
	r.Lock()
	defer r.Unlock()

	var index = -1
	for i, member := range r.members {
		if member.memberInfo.ServiceAddr == addr {
			index = i
			break
		}
	}
	if index != -1 {
		if index == len(r.members)-1 {
			r.members = r.members[:index]
		} else {
			r.members = append(r.members[:index], r.members[index+1:]...)
		}
	}
}
