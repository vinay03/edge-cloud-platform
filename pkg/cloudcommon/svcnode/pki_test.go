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

// Note file package is not node, so avoids node package having
// dependencies on process package.
package svcnode_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edgexr/edge-cloud-platform/api/edgeproto"
	"github.com/edgexr/edge-cloud-platform/pkg/cloudcommon"
	"github.com/edgexr/edge-cloud-platform/pkg/cloudcommon/svcnode"
	"github.com/edgexr/edge-cloud-platform/pkg/log"
	"github.com/edgexr/edge-cloud-platform/pkg/process"
	edgetls "github.com/edgexr/edge-cloud-platform/pkg/tls"
	"github.com/edgexr/edge-cloud-platform/pkg/vault"
	"github.com/edgexr/edge-cloud-platform/test/testutil"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/examples/features/proto/echo"
	"google.golang.org/grpc/grpclog"
)

func TestInternalPki(t *testing.T) {
	log.SetDebugLevel(log.DebugLevelApi)
	log.InitTracer(nil)
	defer log.FinishTracer()
	ctx := log.StartTestSpan(context.Background())
	// grcp logs not showing up in unit tests for some reason.
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(ioutil.Discard, ioutil.Discard, os.Stderr))
	// Generate certs if needed
	certsDir := "/tmp/edge-cloud-pki-test-certs"
	out, err := exec.Command("../../tls/gen-test-certs.sh", "foo-us-ca", "us.ctrl.edgecloud.net", certsDir).CombinedOutput()
	require.Nil(t, err, "%s", string(out))

	// Set up local Vault process.
	// Note that this test depends on the approles and
	// pki configuration done by the vault setup scripts
	// that are run as part of running this Vault process.
	vp := process.Vault{
		Common: process.Common{
			Name: "vault",
		},
		Regions:    "us,eu",
		ListenAddr: "TestInternalPki",
		PKIDomain:  "edgecloud.net",
	}
	_, vroles, vaultCleanup := testutil.NewVaultTestCluster(t, &vp)
	defer vaultCleanup()
	require.Nil(t, err, "start local vault")
	defer vp.StopLocal()

	svcnode.BadAuthDelay = time.Millisecond
	svcnode.VerifyDelay = time.Millisecond
	svcnode.VerifyRetry = 3

	vaultAddr := vp.ListenAddr
	// Set up fake Controller to serve access key API
	dcUS := &DummyController{}
	dcUS.Init(ctx, "us", vroles, vaultAddr)
	dcUS.Start(ctx)
	defer dcUS.Stop()

	// Set up fake Controller to serve access key API
	dcEU := &DummyController{}
	dcEU.Init(ctx, "eu", vroles, vaultAddr)
	dcEU.Start(ctx)
	defer dcEU.Stop()

	// create access key for US cloudlet
	edgeboxCloudlet := true
	tc1 := dcUS.CreateCloudlet(ctx, "pkitc1", !edgeboxCloudlet)
	err = dcUS.UpdateKey(ctx, tc1.Cloudlet.Key)
	require.Nil(t, err)
	// create access key for EU cloudlet
	tc2 := dcEU.CreateCloudlet(ctx, "pkitc2", !edgeboxCloudlet)
	err = dcEU.UpdateKey(ctx, tc2.Cloudlet.Key)
	require.Nil(t, err)

	// Most positive testing is done by e2e tests.

	// Negative testing for issuing certs.
	// These primarily test Vault certificate role permissions,
	// so work in conjunction with the vault setup in vault/setup-region.sh
	// Apparently CA certs are always readable from Vault approles.
	var cfgTests cfgTestList
	// regional Controller cannot issue global cert
	cfgTests.add(ConfigTest{
		NodeType:    svcnode.SvcNodeTypeController,
		Region:      "us",
		LocalIssuer: svcnode.CertIssuerRegional,
		TestIssuer:  svcnode.CertIssuerGlobal,
		ExpectErr:   "write failure pki-global/issue/us",
	})
	// regional Controller can issue RegionalCloudlet, for access-key services.
	cfgTests.add(ConfigTest{
		NodeType:    svcnode.SvcNodeTypeController,
		Region:      "us",
		LocalIssuer: svcnode.CertIssuerRegional,
		TestIssuer:  svcnode.CertIssuerRegionalCloudlet,
		ExpectErr:   "",
	})
	// global node cannot issue regional cert
	cfgTests.add(ConfigTest{
		NodeType:    svcnode.SvcNodeTypeNotifyRoot,
		LocalIssuer: svcnode.CertIssuerGlobal,
		TestIssuer:  svcnode.CertIssuerRegional,
		ExpectErr:   "write failure pki-regional/issue/default",
	})
	// global node cannot issue regional-cloudlet cert
	cfgTests.add(ConfigTest{
		NodeType:    svcnode.SvcNodeTypeNotifyRoot,
		LocalIssuer: svcnode.CertIssuerGlobal,
		TestIssuer:  svcnode.CertIssuerRegionalCloudlet,
		ExpectErr:   "write failure pki-regional-cloudlet/issue/default",
	})
	// cloudlet node cannot issue global cert
	cfgTests.add(ConfigTest{
		NodeType:      svcnode.SvcNodeTypeCRM,
		Region:        "us",
		LocalIssuer:   svcnode.CertIssuerRegionalCloudlet,
		TestIssuer:    svcnode.CertIssuerGlobal,
		AccessKeyFile: tc1.KeyClient.AccessKeyFile,
		AccessApiAddr: tc1.KeyClient.AccessApiAddr,
		CloudletKey:   &tc1.Cloudlet.Key,
		ExpectErr:     "Controller will only issue RegionalCloudlet certs",
	})
	// cloudlet node cannot issue regional cert
	cfgTests.add(ConfigTest{
		NodeType:      svcnode.SvcNodeTypeCRM,
		Region:        "us",
		LocalIssuer:   svcnode.CertIssuerRegionalCloudlet,
		TestIssuer:    svcnode.CertIssuerRegional,
		AccessKeyFile: tc1.KeyClient.AccessKeyFile,
		AccessApiAddr: tc1.KeyClient.AccessApiAddr,
		CloudletKey:   &tc1.Cloudlet.Key,
		ExpectErr:     "Controller will only issue RegionalCloudlet certs",
	})
	// cloudlet node can issue RegionalCloudlet cert
	cfgTests.add(ConfigTest{
		NodeType:      svcnode.SvcNodeTypeCRM,
		Region:        "us",
		LocalIssuer:   svcnode.CertIssuerRegionalCloudlet,
		TestIssuer:    svcnode.CertIssuerRegionalCloudlet,
		AccessKeyFile: tc1.KeyClient.AccessKeyFile,
		AccessApiAddr: tc1.KeyClient.AccessApiAddr,
		CloudletKey:   &tc1.Cloudlet.Key,
		ExpectErr:     "",
	})

	for _, cfg := range cfgTests {
		testGetTlsConfig(t, ctx, vaultAddr, vroles, &cfg)
	}

	// define nodes for certificate exchange tests
	notifyRootServer := &PkiConfig{
		Type:        svcnode.SvcNodeTypeNotifyRoot,
		LocalIssuer: svcnode.CertIssuerGlobal,
		UseVaultPki: true,
		RemoteCAs: []svcnode.MatchCA{
			svcnode.AnyRegionalMatchCA(),
			svcnode.GlobalMatchCA(),
		},
	}
	controllerClientUS := &PkiConfig{
		Region:      "us",
		Type:        svcnode.SvcNodeTypeController,
		LocalIssuer: svcnode.CertIssuerRegional,
		UseVaultPki: true,
		RemoteCAs: []svcnode.MatchCA{
			svcnode.GlobalMatchCA(),
		},
	}
	controllerServerUS := &PkiConfig{
		Region:      "us",
		Type:        svcnode.SvcNodeTypeController,
		LocalIssuer: svcnode.CertIssuerRegional,
		UseVaultPki: true,
		RemoteCAs: []svcnode.MatchCA{
			svcnode.SameRegionalMatchCA(),
			svcnode.SameRegionalCloudletMatchCA(),
		},
	}
	controllerApiServerUS := &PkiConfig{
		Region:      "us",
		Type:        svcnode.SvcNodeTypeController,
		LocalIssuer: svcnode.CertIssuerRegional,
		UseVaultPki: true,
		RemoteCAs: []svcnode.MatchCA{
			svcnode.GlobalMatchCA(),
			svcnode.SameRegionalMatchCA(),
		},
	}
	controllerApiServerEU := &PkiConfig{
		Region:      "eu",
		Type:        svcnode.SvcNodeTypeController,
		LocalIssuer: svcnode.CertIssuerRegional,
		UseVaultPki: true,
		RemoteCAs: []svcnode.MatchCA{
			svcnode.GlobalMatchCA(),
			svcnode.SameRegionalMatchCA(),
		},
	}
	crmClientUS := &PkiConfig{
		Region:        "us",
		Type:          svcnode.SvcNodeTypeCRM,
		LocalIssuer:   svcnode.CertIssuerRegionalCloudlet,
		UseVaultPki:   true,
		AccessKeyFile: tc1.KeyClient.AccessKeyFile,
		AccessApiAddr: tc1.KeyClient.AccessApiAddr,
		CloudletKey:   &tc1.Cloudlet.Key,
		RemoteCAs: []svcnode.MatchCA{
			svcnode.SameRegionalMatchCA(),
		},
	}
	crmClientEU := &PkiConfig{
		Region:        "eu",
		Type:          svcnode.SvcNodeTypeCRM,
		LocalIssuer:   svcnode.CertIssuerRegionalCloudlet,
		UseVaultPki:   true,
		AccessKeyFile: tc2.KeyClient.AccessKeyFile,
		AccessApiAddr: tc2.KeyClient.AccessApiAddr,
		CloudletKey:   &tc2.Cloudlet.Key,
		RemoteCAs: []svcnode.MatchCA{
			svcnode.SameRegionalMatchCA(),
		},
	}
	dmeClientRegionalUS := &PkiConfig{
		Region:      "us",
		Type:        svcnode.SvcNodeTypeDME,
		LocalIssuer: svcnode.CertIssuerRegional,
		UseVaultPki: true,
		RemoteCAs: []svcnode.MatchCA{
			svcnode.SameRegionalMatchCA(),
		},
	}
	mc := &PkiConfig{
		Type:        svcnode.SvcNodeTypeNotifyRoot,
		LocalIssuer: svcnode.CertIssuerGlobal,
		UseVaultPki: true,
		RemoteCAs: []svcnode.MatchCA{
			svcnode.AnyRegionalMatchCA(),
		},
	}
	// assume attacker stole crm EU certs, and vault login
	// so has regional-cloudlet cert and can pull all CAs.
	crmRogueEU := &PkiConfig{
		Region:        "eu",
		Type:          svcnode.SvcNodeTypeCRM,
		LocalIssuer:   svcnode.CertIssuerRegionalCloudlet,
		UseVaultPki:   true,
		AccessKeyFile: tc2.KeyClient.AccessKeyFile,
		AccessApiAddr: tc2.KeyClient.AccessApiAddr,
		CloudletKey:   &tc2.Cloudlet.Key,
		RemoteCAs: []svcnode.MatchCA{
			svcnode.GlobalMatchCA(),
			svcnode.AnyRegionalMatchCA(),
			svcnode.SameRegionalMatchCA(),
			svcnode.SameRegionalCloudletMatchCA(),
		},
	}
	edgeTurnEU := &PkiConfig{
		Region:      "eu",
		Type:        svcnode.SvcNodeTypeEdgeTurn,
		LocalIssuer: svcnode.CertIssuerRegional,
		UseVaultPki: true,
		RemoteCAs: []svcnode.MatchCA{
			svcnode.SameRegionalCloudletMatchCA(),
		},
	}
	edgeTurnUS := &PkiConfig{
		Region:      "us",
		Type:        svcnode.SvcNodeTypeEdgeTurn,
		LocalIssuer: svcnode.CertIssuerRegional,
		UseVaultPki: true,
		RemoteCAs: []svcnode.MatchCA{
			svcnode.SameRegionalCloudletMatchCA(),
		},
	}

	// Testing for certificate exchange.
	var csTests clientServerList
	// controller can connect to notifyroot
	csTests.add(ClientServer{
		Server: notifyRootServer,
		Client: controllerClientUS,
	})
	// mc can connect to controller
	csTests.add(ClientServer{
		Server: controllerApiServerUS,
		Client: mc,
	})
	csTests.add(ClientServer{
		Server: controllerApiServerEU,
		Client: mc,
	})
	// crm can connect to controller
	csTests.add(ClientServer{
		Server: controllerServerUS,
		Client: crmClientUS,
	})
	// crm from EU cannot connect to US controller
	csTests.add(ClientServer{
		Server:          controllerServerUS,
		Client:          crmClientEU,
		ExpectClientErr: "region mismatch",
		ExpectServerErr: "remote error: tls: bad certificate",
	})
	// crm cannot connect to notifyroot
	csTests.add(ClientServer{
		Server:          notifyRootServer,
		Client:          crmClientUS,
		ExpectClientErr: "certificate signed by unknown authority",
		ExpectServerErr: "remote error: tls: bad certificate",
	})
	// crm can connect to edgeturn
	csTests.add(ClientServer{
		Server: edgeTurnUS,
		Client: crmClientUS,
	})
	// crm from US cannot connect to EU edgeturn
	csTests.add(ClientServer{
		Server:          edgeTurnEU,
		Client:          crmClientUS,
		ExpectClientErr: "region mismatch",
		ExpectServerErr: "remote error: tls: bad certificate",
	})
	// crm from EU cannot connect to US edgeturn
	csTests.add(ClientServer{
		Server:          edgeTurnUS,
		Client:          crmClientEU,
		ExpectClientErr: "region mismatch",
		ExpectServerErr: "remote error: tls: bad certificate",
	})
	// rogue crm cannot connect to notify root
	csTests.add(ClientServer{
		Server:          notifyRootServer,
		Client:          crmRogueEU,
		ExpectClientErr: "remote error: tls: bad certificate",
		ExpectServerErr: "certificate signed by unknown authority",
	})
	// rogue crm cannot connect to other region controller
	csTests.add(ClientServer{
		Server:          controllerServerUS,
		Client:          crmRogueEU,
		ExpectClientErr: "region mismatch",
		ExpectServerErr: "remote error: tls: bad certificate",
	})
	// rogue crm cannot pretend to be controller
	csTests.add(ClientServer{
		Server:          crmRogueEU,
		Client:          crmClientEU,
		ExpectClientErr: "certificate signed by unknown authority",
		ExpectServerErr: "remote error: tls: bad certificate",
	})
	// rogue crm cannot pretend to be notifyroot
	csTests.add(ClientServer{
		Server:          crmRogueEU,
		Client:          controllerClientUS,
		ExpectClientErr: "certificate signed by unknown authority",
		ExpectServerErr: "remote error: tls: bad certificate",
	})

	// These test config options and rollout phases
	nodeNoTls := &PkiConfig{
		Region: "us",
		Type:   svcnode.SvcNodeTypeController,
	}
	nodeFileOnly := &PkiConfig{
		Region:   "us",
		Type:     svcnode.SvcNodeTypeController,
		CertFile: certsDir + "/out/us.ctrl.edgecloud.net.crt",
		CertKey:  certsDir + "/out/us.ctrl.edgecloud.net.key",
		CAFile:   certsDir + "/out/foo-us-ca.crt",
	}
	nodePhase2 := &PkiConfig{
		Region:      "us",
		Type:        svcnode.SvcNodeTypeController,
		CertFile:    certsDir + "/out/us.ctrl.edgecloud.net.crt",
		CertKey:     certsDir + "/out/us.ctrl.edgecloud.net.key",
		CAFile:      certsDir + "/out/foo-us-ca.crt",
		UseVaultPki: true,
		LocalIssuer: svcnode.CertIssuerRegional,
		RemoteCAs: []svcnode.MatchCA{
			svcnode.SameRegionalMatchCA(),
		},
	}
	nodePhase3 := &PkiConfig{
		Region:      "us",
		Type:        svcnode.SvcNodeTypeController,
		UseVaultPki: true,
		LocalIssuer: svcnode.CertIssuerRegional,
		RemoteCAs: []svcnode.MatchCA{
			svcnode.SameRegionalMatchCA(),
		},
	}
	// local testing
	csTests.add(ClientServer{
		Server: nodeNoTls,
		Client: nodeNoTls,
	})
	// existing
	csTests.add(ClientServer{
		Server: nodeFileOnly,
		Client: nodeFileOnly,
	})
	// phase3
	csTests.add(ClientServer{
		Server: nodePhase3,
		Client: nodePhase3,
	})
	csTests.add(ClientServer{
		Server: nodePhase2,
		Client: nodePhase3,
	})
	csTests.add(ClientServer{
		Server: nodePhase3,
		Client: nodePhase2,
	})
	csTests.add(ClientServer{
		Server:          nodePhase3,
		Client:          nodeFileOnly,
		ExpectClientErr: "certificate signed by unknown authority",
		ExpectServerErr: "remote error: tls: bad certificate",
	})

	for _, test := range csTests {
		testExchange(t, ctx, vaultAddr, vroles, &test)
	}

	// Tests for Tls interceptor that allows access for Global/Regional
	// clients, but requires an additional access key for RegionalCloudlet.
	// This will be used on the notify API.
	var ccTests clientControllerList
	// crm can connect within same region
	ccTests.add(ClientController{
		Controller:                 dcUS,
		Client:                     crmClientUS,
		ControllerRequireAccessKey: true,
	})
	ccTests.add(ClientController{
		Controller:                 dcEU,
		Client:                     crmClientEU,
		ControllerRequireAccessKey: true,
	})
	// crm cannot connect to different region
	ccTests.add(ClientController{
		Controller:                 dcUS,
		Client:                     crmClientEU,
		ControllerRequireAccessKey: true,
		ExpectErr:                  "region mismatch, expected local uri sans for eu",
	})
	ccTests.add(ClientController{
		Controller:                 dcEU,
		Client:                     crmClientUS,
		ControllerRequireAccessKey: true,
		ExpectErr:                  "region mismatch, expected local uri sans for us",
	})
	// test invalid keys
	ccTests.add(ClientController{
		Controller:                 dcUS,
		Client:                     crmClientUS,
		ControllerRequireAccessKey: true,
		InvalidateClientKey:        true,
		ExpectErr:                  "Failed to verify cloudlet access key signature",
	})
	ccTests.add(ClientController{
		Controller:                 dcEU,
		Client:                     crmClientEU,
		ControllerRequireAccessKey: true,
		InvalidateClientKey:        true,
		ExpectErr:                  "Failed to verify cloudlet access key signature",
	})
	// ignore invalid keys or missing keys for backwards compatibility
	// with CRMs that were not upgraded
	ccTests.add(ClientController{
		Controller:                 dcUS,
		Client:                     crmClientUS,
		ControllerRequireAccessKey: false,
		InvalidateClientKey:        true,
	})
	ccTests.add(ClientController{
		Controller:                 dcEU,
		Client:                     crmClientEU,
		ControllerRequireAccessKey: false,
		InvalidateClientKey:        true,
	})
	// same regional service is ok (regional dme, etc)
	ccTests.add(ClientController{
		Controller: dcUS,
		Client:     dmeClientRegionalUS,
	})
	for _, test := range ccTests {
		testTlsConnect(t, ctx, &test, vaultAddr)
	}
}

type ConfigTest struct {
	NodeType      string
	Region        string
	LocalIssuer   string
	TestIssuer    string
	RemoteCAs     []svcnode.MatchCA
	ExpectErr     string
	Line          string
	AccessKeyFile string
	AccessApiAddr string
	CloudletKey   *edgeproto.CloudletKey
}

func testGetTlsConfig(t *testing.T, ctx context.Context, vaultAddr string, vroles *process.VaultRoles, cfg *ConfigTest) {
	log.SpanLog(ctx, log.DebugLevelInfo, "run testGetTlsConfig", "cfg", cfg)
	vc := getVaultConfig(cfg.NodeType, cfg.Region, vaultAddr, vroles)
	mgr := svcnode.SvcNodeMgr{}
	mgr.InternalPki.UseVaultPki = true
	mgr.ValidDomains = "edgecloud.net"
	if cfg.AccessKeyFile != "" && cfg.AccessApiAddr != "" {
		mgr.AccessKeyClient.AccessKeyFile = cfg.AccessKeyFile
		mgr.AccessKeyClient.AccessApiAddr = cfg.AccessApiAddr
		mgr.AccessKeyClient.TestSkipTlsVerify = true
	}
	// nodeMgr init will attempt to issue a cert to be able to talk
	// to Jaeger/ElasticSearch
	opts := []svcnode.NodeOp{
		svcnode.WithRegion(cfg.Region),
		svcnode.WithVaultConfig(vc),
	}
	if cfg.CloudletKey != nil {
		opts = append(opts, svcnode.WithCloudletKey(cfg.CloudletKey))
	}
	_, _, err := mgr.Init(cfg.NodeType, cfg.LocalIssuer, opts...)
	errStr := ""
	if err != nil {
		errStr = err.Error()
	}
	require.Nil(t, err, "nodeMgr init %s, %s", cfg.Line, errStr)
	_, err = mgr.InternalPki.GetServerTlsConfig(ctx,
		mgr.CommonNamePrefix(),
		cfg.TestIssuer,
		cfg.RemoteCAs)
	if cfg.ExpectErr == "" {
		require.Nil(t, err, "get tls config %s", cfg.Line)
	} else {
		require.NotNil(t, err, "get tls config %s", cfg.Line)
		require.Contains(t, err.Error(), cfg.ExpectErr, "get tls config %s", cfg.Line)
	}
}

type PkiConfig struct {
	Region        string
	Type          string
	LocalIssuer   string
	CertFile      string
	CertKey       string
	CAFile        string
	UseVaultPki   bool
	RemoteCAs     []svcnode.MatchCA
	AccessKeyFile string
	AccessApiAddr string
	CloudletKey   *edgeproto.CloudletKey
}

type ClientServer struct {
	Server          *PkiConfig
	Client          *PkiConfig
	ExpectServerErr string
	ExpectClientErr string
	Line            string
}

func (s *PkiConfig) setupNodeMgr(vaultAddr string, vroles *process.VaultRoles) (*svcnode.SvcNodeMgr, error) {
	vaultCfg := getVaultConfig(s.Type, s.Region, vaultAddr, vroles)
	nodeMgr := svcnode.SvcNodeMgr{}
	nodeMgr.SetInternalTlsCertFile(s.CertFile)
	nodeMgr.SetInternalTlsKeyFile(s.CertKey)
	nodeMgr.SetInternalTlsCAFile(s.CAFile)
	nodeMgr.InternalPki.UseVaultPki = s.UseVaultPki
	nodeMgr.ValidDomains = "edgecloud.net"
	if s.AccessKeyFile != "" && s.AccessApiAddr != "" {
		nodeMgr.AccessKeyClient.AccessKeyFile = s.AccessKeyFile
		nodeMgr.AccessKeyClient.AccessApiAddr = s.AccessApiAddr
		nodeMgr.AccessKeyClient.TestSkipTlsVerify = true
	}
	opts := []svcnode.NodeOp{
		svcnode.WithRegion(s.Region),
		svcnode.WithVaultConfig(vaultCfg),
	}
	if s.CloudletKey != nil {
		opts = append(opts, svcnode.WithCloudletKey(s.CloudletKey))
	}
	_, _, err := nodeMgr.Init(s.Type, s.LocalIssuer, opts...)
	return &nodeMgr, err
}

func testExchange(t *testing.T, ctx context.Context, vaultAddr string, vroles *process.VaultRoles, cs *ClientServer) {
	if !strings.HasPrefix(runtime.Version(), "go1.12") {
		// After go1.12, the client side Dial/Handshake does not return
		// error when the server decides to abort the connection.
		// Only the server side returns an error.
		if cs.ExpectClientErr == "remote error: tls: bad certificate" {
			cs.ExpectClientErr = ""
		}
	}
	fmt.Printf("******************* testExchange %s *********************\n", cs.Line)
	serverNode, err := cs.Server.setupNodeMgr(vaultAddr, vroles)
	require.Nil(t, err, "serverNode init %s", cs.Line)
	serverTls, err := serverNode.InternalPki.GetServerTlsConfig(ctx,
		serverNode.CommonNamePrefix(),
		cs.Server.LocalIssuer,
		cs.Server.RemoteCAs)
	require.Nil(t, err, "get server tls config %s", cs.Line)
	if cs.Server.CertFile != "" || cs.Server.UseVaultPki {
		require.NotNil(t, serverTls)
	}

	clientNode, err := cs.Client.setupNodeMgr(vaultAddr, vroles)
	require.Nil(t, err, "clientNode init %s", cs.Line)
	clientTls, err := clientNode.InternalPki.GetClientTlsConfig(ctx,
		clientNode.CommonNamePrefix(),
		cs.Client.LocalIssuer,
		cs.Client.RemoteCAs)
	require.Nil(t, err, "get client tls config %s", cs.Line)
	if cs.Client.CertFile != "" || cs.Client.UseVaultPki {
		require.NotNil(t, clientTls)
		// must set ServerName due to the way this test is set up
		clientTls.ServerName = serverNode.CommonNames()[0]
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.Nil(t, err)
	defer lis.Close()

	connDone := make(chan bool, 2)
	// loop twice so we can test cert refresh
	for i := 0; i < 2; i++ {
		var serr error
		go func() {
			var sconn net.Conn
			sconn, serr = lis.Accept()
			defer sconn.Close()
			if serverTls != nil {
				srv := tls.Server(sconn, serverTls)
				serr = srv.Handshake()
			}
			connDone <- true
		}()
		log.SpanLog(ctx, log.DebugLevelInfo, "client dial", "addr", lis.Addr().String())
		var err error
		if clientTls == nil {
			var conn net.Conn
			conn, err = net.Dial("tcp", lis.Addr().String())
			if err == nil {
				defer conn.Close()
			}
		} else {
			var conn *tls.Conn
			conn, err = tls.Dial("tcp", lis.Addr().String(), clientTls)
			if err == nil {
				defer conn.Close()
				err = conn.Handshake()
			}
		}
		<-connDone
		if cs.ExpectClientErr == "" {
			require.Nil(t, err, "client dial/handshake [%d] %s", i, cs.Line)
		} else {
			require.NotNil(t, err, "client dial [%d] %s", i, cs.Line)
			require.Contains(t, err.Error(), cs.ExpectClientErr, "client error check for [%d] %s", i, cs.Line)
		}
		if cs.ExpectServerErr == "" {
			require.Nil(t, serr, "server dial/handshake [%d] %s", i, cs.Line)
		} else {
			require.NotNil(t, serr, "server accept [%d] %s", i, cs.Line)
			require.Contains(t, serr.Error(), cs.ExpectServerErr, "server error check for [%d] %s", i, cs.Line)
		}
		if i == 1 {
			// no need to refresh on last iteration
			break
		}
		// refresh certs. same tls config should pick up new certs.
		err = serverNode.InternalPki.RefreshNow(ctx)
		require.Nil(t, err, "refresh server certs [%d] %s", i, cs.Line)
		err = clientNode.InternalPki.RefreshNow(ctx)
		require.Nil(t, err, "refresh client certs [%d] %s", i, cs.Line)
	}
}

type ClientController struct {
	Controller                 *DummyController
	Client                     *PkiConfig
	ControllerRequireAccessKey bool
	InvalidateClientKey        bool
	ExpectErr                  string
	Line                       string
}

func testTlsConnect(t *testing.T, ctx context.Context, cc *ClientController, vaultAddr string) {
	// This tests the TLS interceptors that will require
	// access keys if client uses the RegionalCloudlet cert.
	fmt.Printf("******************* testTlsConnect %s *********************\n", cc.Line)
	cc.Controller.DummyController.KeyServer.SetRequireTlsAccessKey(cc.ControllerRequireAccessKey)

	clientNode, err := cc.Client.setupNodeMgr(vaultAddr, cc.Controller.vroles)
	require.Nil(t, err, "clientNode init %s", cc.Line)
	clientTls, err := clientNode.InternalPki.GetClientTlsConfig(ctx,
		clientNode.CommonNamePrefix(),
		cc.Client.LocalIssuer,
		cc.Client.RemoteCAs)
	require.Nil(t, err, "get client tls config %s", cc.Line)
	if cc.Client.CertFile != "" || cc.Client.UseVaultPki {
		require.NotNil(t, clientTls)
		// must set ServerName due to the way this test is set up
		clientTls.ServerName = cc.Controller.nodeMgr.CommonNames()[0]
	}
	// for negative testing with invalid key
	if cc.InvalidateClientKey && cc.Client.CloudletKey != nil {
		cc.Controller.UpdateKey(ctx, *cc.Client.CloudletKey)
	}
	// interceptors will add access key to grpc metadata if access key present
	unaryInterceptor := log.UnaryClientTraceGrpc
	if cc.Client.AccessKeyFile != "" {
		unaryInterceptor = grpc_middleware.ChainUnaryClient(
			log.UnaryClientTraceGrpc,
			clientNode.AccessKeyClient.UnaryAddAccessKey)
	}
	streamInterceptor := log.StreamClientTraceGrpc
	if cc.Client.AccessKeyFile != "" {
		streamInterceptor = grpc_middleware.ChainStreamClient(
			log.StreamClientTraceGrpc,
			clientNode.AccessKeyClient.StreamAddAccessKey)
	}

	clientConn, err := grpc.Dial(cc.Controller.TlsAddr(),
		edgetls.GetGrpcDialOption(clientTls),
		grpc.WithUnaryInterceptor(unaryInterceptor),
		grpc.WithStreamInterceptor(streamInterceptor),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(&cloudcommon.ProtoCodec{})),
	)
	require.Nil(t, err, "create client conn %s", cc.Line)
	svcnode.EchoApisTest(t, ctx, clientConn, cc.ExpectErr)
}

func getVaultConfig(nodetype, region, addr string, vroles *process.VaultRoles) *vault.Config {
	var roleid string
	var secretid string

	if nodetype == svcnode.SvcNodeTypeNotifyRoot {
		roleid = vroles.NotifyRootRoleID
		secretid = vroles.NotifyRootSecretID
	} else {
		if region == "" {
			// for testing, map to us region"
			region = "us"
		}
		rr := vroles.GetRegionRoles(region)
		if rr == nil {
			panic("no roles for region")
		}
		switch nodetype {
		case svcnode.SvcNodeTypeDME:
			roleid = rr.DmeRoleID
			secretid = rr.DmeSecretID
		case svcnode.SvcNodeTypeCRM:
			// no vault access for crm
			return nil
		case svcnode.SvcNodeTypeController:
			roleid = rr.CtrlRoleID
			secretid = rr.CtrlSecretID
		case svcnode.SvcNodeTypeAddonMgr:
			fallthrough
		case svcnode.SvcNodeTypeClusterSvc:
			roleid = rr.ClusterSvcRoleID
			secretid = rr.ClusterSvcSecretID
		case svcnode.SvcNodeTypeEdgeTurn:
			roleid = rr.EdgeTurnRoleID
			secretid = rr.EdgeTurnSecretID
		default:
			panic("invalid node type")
		}
	}
	auth := vault.NewAppRoleAuth(roleid, secretid)
	return vault.NewConfig(addr, auth)
}

// Track line number of objects added to list to make it easier
// to debug if one of them fails.

type cfgTestList []ConfigTest

func (list *cfgTestList) add(cfg ConfigTest) {
	_, file, line, _ := runtime.Caller(1)
	cfg.Line = fmt.Sprintf("%s:%d", filepath.Base(file), line)
	*list = append(*list, cfg)
}

type clientServerList []ClientServer

func (list *clientServerList) add(cs ClientServer) {
	_, file, line, _ := runtime.Caller(1)
	cs.Line = fmt.Sprintf("%s:%d", filepath.Base(file), line)
	*list = append(*list, cs)
}

type clientControllerList []ClientController

func (list *clientControllerList) add(cc ClientController) {
	_, file, line, _ := runtime.Caller(1)
	cc.Line = fmt.Sprintf("%s:%d", filepath.Base(file), line)
	*list = append(*list, cc)
}

// Dummy controller serves Vault certs to access key clients
type DummyController struct {
	svcnode.DummyController
	nodeMgr       svcnode.SvcNodeMgr
	vroles        *process.VaultRoles
	TlsLis        net.Listener
	TlsServ       *grpc.Server
	TlsRegisterCb func(server *grpc.Server)
}

func (s *DummyController) Init(ctx context.Context, region string, vroles *process.VaultRoles, vaultAddr string) error {
	s.DummyController.Init(vaultAddr)
	s.DummyController.RegisterCloudletAccess = false // register it here
	s.DummyController.ApiRegisterCb = func(serv *grpc.Server) {
		// add APIs to issue certs to CRM/etc
		edgeproto.RegisterCloudletAccessApiServer(serv, s)
	}
	es := &svcnode.EchoServer{}
	s.TlsRegisterCb = func(serv *grpc.Server) {
		// echo server for testing
		echo.RegisterEchoServer(serv, es)
	}
	// no crm vault role/secret env vars for controller (no backwards compatability)
	s.vroles = vroles

	vc := getVaultConfig(svcnode.SvcNodeTypeController, region, vaultAddr, vroles)
	s.nodeMgr.InternalPki.UseVaultPki = true
	s.nodeMgr.ValidDomains = "edgecloud.net"
	_, _, err := s.nodeMgr.Init(svcnode.SvcNodeTypeController, svcnode.NoTlsClientIssuer, svcnode.WithRegion(region), svcnode.WithVaultConfig(vc))
	return err
}

func (s *DummyController) Start(ctx context.Context) {
	s.DummyController.Start(ctx, "127.0.0.1:0")
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err.Error())
	}
	s.TlsLis = lis
	// same config as Controller's notify server
	tlsConfig, err := s.nodeMgr.InternalPki.GetServerTlsConfig(ctx,
		s.nodeMgr.CommonNamePrefix(),
		svcnode.CertIssuerRegional,
		[]svcnode.MatchCA{
			svcnode.SameRegionalMatchCA(),
			svcnode.SameRegionalCloudletMatchCA(),
		})
	if err != nil {
		panic(err.Error())
	}
	// The "tls" interceptors, which only require an access-key
	// if the client is using a RegionalCloudlet cert.
	s.TlsServ = grpc.NewServer(
		cloudcommon.GrpcCreds(tlsConfig),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			cloudcommon.AuditUnaryInterceptor,
			s.KeyServer.UnaryTlsAccessKey,
		)),
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
			cloudcommon.AuditStreamInterceptor,
			s.KeyServer.StreamTlsAccessKey,
		)),
		grpc.ForceServerCodec(&cloudcommon.ProtoCodec{}),
	)
	if s.TlsRegisterCb != nil {
		s.TlsRegisterCb(s.TlsServ)
	}
	go func() {
		err := s.TlsServ.Serve(s.TlsLis)
		if err != nil {
			panic(err.Error())
		}
	}()
}

func (s *DummyController) Stop() {
	s.DummyController.Stop()
	s.TlsServ.Stop()
	s.TlsLis.Close()
}

func (s *DummyController) TlsAddr() string {
	return s.TlsLis.Addr().String()
}

func (s *DummyController) IssueCert(ctx context.Context, req *edgeproto.IssueCertRequest) (*edgeproto.IssueCertReply, error) {
	log.SpanLog(ctx, log.DebugLevelApi, "dummy controller issue cert", "req", req)
	reply := &edgeproto.IssueCertReply{}
	certId := svcnode.CertId{
		CommonNamePrefix: req.CommonNamePrefix,
		Issuer:           svcnode.CertIssuerRegionalCloudlet,
	}
	vc, err := s.nodeMgr.InternalPki.IssueVaultCertDirect(ctx, certId)
	if err != nil {
		return reply, err
	}
	reply.PublicCertPem = string(vc.PublicCertPEM)
	reply.PrivateKeyPem = string(vc.PrivateKeyPEM)
	return reply, nil
}

func (s *DummyController) GetCas(ctx context.Context, req *edgeproto.GetCasRequest) (*edgeproto.GetCasReply, error) {
	log.SpanLog(ctx, log.DebugLevelApi, "dummy controller get cas", "req", req)
	reply := &edgeproto.GetCasReply{}
	cab, err := s.nodeMgr.InternalPki.GetVaultCAsDirect(ctx, req.Issuer)
	if err != nil {
		return reply, err
	}
	reply.CaChainPem = string(cab)
	return reply, err
}

func (s *DummyController) GetAccessData(ctx context.Context, in *edgeproto.AccessDataRequest) (*edgeproto.AccessDataReply, error) {
	return &edgeproto.AccessDataReply{}, nil
}
