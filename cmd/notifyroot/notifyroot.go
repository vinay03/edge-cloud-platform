// Copyright 2022 MobiledgeX, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/edgexr/edge-cloud-platform/api/edgeproto"
	"github.com/edgexr/edge-cloud-platform/pkg/cloudcommon/svcnode"
	"github.com/edgexr/edge-cloud-platform/pkg/log"
	"github.com/edgexr/edge-cloud-platform/pkg/notify"
	"google.golang.org/grpc"
)

var notifyAddr = flag.String("notifyAddr", "127.0.0.1:53001", "Notify listener address")
var debugLevels = flag.String("d", "", fmt.Sprintf("comma separated list of %v", log.DebugLevelStrings))

var nodeMgr svcnode.SvcNodeMgr
var sigChan chan os.Signal

func main() {
	nodeMgr.InitFlags()
	flag.Parse()
	log.SetDebugLevelStrs(*debugLevels)

	ctx, span, err := nodeMgr.Init(svcnode.SvcNodeTypeNotifyRoot, svcnode.CertIssuerGlobal)
	if err != nil {
		log.FatalLog("Failed to init node", "err", err)
	}
	defer nodeMgr.Finish()

	notifyServer := &notify.ServerMgr{}
	nodeMgr.RegisterServer(notifyServer)
	notifyServer.RegisterServerCb(func(s *grpc.Server) {
		edgeproto.RegisterSvcNodeApiServer(s, &svcNodeApi)
		edgeproto.RegisterDebugApiServer(s, &debugApi)
		edgeproto.RegisterAppInstLatencyApiServer(s, &appInstLatencyApi)
	})

	tlsConfig, err := nodeMgr.InternalPki.GetServerTlsConfig(ctx,
		nodeMgr.CommonNamePrefix(),
		svcnode.CertIssuerGlobal,
		[]svcnode.MatchCA{
			svcnode.AnyRegionalMatchCA(),
			svcnode.GlobalMatchCA(),
		})
	if err != nil {
		log.FatalLog("Failed to get tls config", "err", err)
	}
	notifyServer.Start(nodeMgr.Name(), *notifyAddr, tlsConfig)
	defer notifyServer.Stop()

	sigChan = make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	log.SpanLog(ctx, log.DebugLevelInfo, "Ready")
	span.Finish()
	sig := <-sigChan
	fmt.Println(sig)
}
