/*
Copyright 2021 The Everoute Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rpcserver

import (
	"net"
	"os"

	"google.golang.org/grpc"
	"k8s.io/klog"

	"github.com/everoute/everoute/pkg/agent/datapath"
	"github.com/everoute/everoute/pkg/apis/rpc/v1alpha1"
	"github.com/everoute/everoute/pkg/constants"
)

type Server struct {
	dpManager *datapath.DpManager
	stopChan  <-chan struct{}
}

func Initialize(datapathManager *datapath.DpManager) *Server {
	s := &Server{
		dpManager: datapathManager,
	}

	return s
}

func (s *Server) Run(stopChan <-chan struct{}) {
	klog.Info("Starting Everoute RPC Server")
	s.stopChan = stopChan

	// create path
	if _, err := os.Stat(constants.EverouteLibPath); os.IsNotExist(err) {
		if err := os.MkdirAll(constants.EverouteLibPath, os.ModePerm); err != nil {
			klog.Fatalf("unable to create %s", constants.EverouteLibPath)
		}
		if err := os.Chmod(constants.EverouteLibPath, os.ModePerm); err != nil {
			klog.Fatalf("unable to chmod %s", constants.EverouteLibPath)
		}
	}

	// remove the remaining sock file
	_, err := os.Stat(constants.RPCSocketAddr)
	if err == nil {
		err = os.Remove(constants.RPCSocketAddr)
		if err != nil {
			klog.Fatalf("remove remaining sock file error, err:%s", err)
			return
		}
	}

	// listen socket
	listener, err := net.Listen("unix", constants.RPCSocketAddr)
	if err != nil {
		klog.Fatalf("Failed to bind on %s: %v", constants.RPCSocketAddr, err)
	}

	rpcServer := grpc.NewServer()
	// register collector service
	collector := NewCollectorServer(s.dpManager, stopChan)
	getterServer := NewGetterServer(s.dpManager)

	v1alpha1.RegisterCollectorServer(rpcServer, collector)
	v1alpha1.RegisterGetterServer(rpcServer, getterServer)

	// start rpc Server
	go func() {
		if err = rpcServer.Serve(listener); err != nil {
			klog.Fatalf("Failed to serve collectorServer connections: %v", err)
		}
	}()

	klog.Info("RPC server is listening ...")
	<-s.stopChan
}