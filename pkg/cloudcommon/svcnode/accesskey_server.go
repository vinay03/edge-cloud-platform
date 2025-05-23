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

package svcnode

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/edgexr/edge-cloud-platform/api/edgeproto"
	"github.com/edgexr/edge-cloud-platform/pkg/cloudcommon"
	"github.com/edgexr/edge-cloud-platform/pkg/log"
	"github.com/edgexr/edge-cloud-platform/pkg/process"
	"github.com/edgexr/edge-cloud-platform/pkg/vault"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"golang.org/x/crypto/ed25519"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// Store and retrieve verified CloudletKey on context

var BadAuthDelay = 3 * time.Second
var UpgradeAccessKeyMethod = "/edgeproto.CloudletAccessKeyApi/UpgradeAccessKey"
var GetAccessDataMethod = "/edgeproto.CloudletAccessApi/GetAccessData"

type accessKeyVerifiedTagType string

const accessKeyVerifiedTag accessKeyVerifiedTagType = "accessKeyVerified"

type AccessKeyVerified struct {
	Key             edgeproto.CloudletKey
	UpgradeRequired bool
}

type AccessKeyCommitFunc func(ctx context.Context, key *edgeproto.CloudletKey, pubPEM string, role process.HARole) error

func ContextSetAccessKeyVerified(ctx context.Context, info *AccessKeyVerified) context.Context {
	return context.WithValue(ctx, accessKeyVerifiedTag, info)
}

func ContextGetAccessKeyVerified(ctx context.Context) *AccessKeyVerified {
	key, ok := ctx.Value(accessKeyVerifiedTag).(*AccessKeyVerified)
	if !ok {
		return nil
	}
	return key
}

// AccessKeyServer maintains state to validate clients.
type AccessKeyServer struct {
	cloudletCache       *edgeproto.CloudletCache
	vaultAddr           string
	requireTlsAccessKey bool
}

func NewAccessKeyServer(cloudletCache *edgeproto.CloudletCache, vaultAddr string) *AccessKeyServer {
	server := &AccessKeyServer{
		cloudletCache: cloudletCache,
		vaultAddr:     vaultAddr,
	}
	// for testing, reduce bad auth delay
	if e2e := os.Getenv("E2ETEST_TLS"); e2e != "" {
		BadAuthDelay = time.Millisecond
	}
	return server
}

func (s *AccessKeyServer) SetRequireTlsAccessKey(require bool) {
	s.requireTlsAccessKey = require
}

func (s *AccessKeyServer) verifyPublicKey(ctx context.Context, pubKeyStr string, message string, sig []byte) error {
	// public key is saved as PEM
	pubKey, err := LoadPubPEM([]byte(pubKeyStr))
	if err != nil {
		return fmt.Errorf("Failed to decode crm public access key, %s, %s", pubKey, err)
	}
	ok := ed25519.Verify(pubKey, []byte(message), sig)
	if !ok {
		return fmt.Errorf("Failed to verify cloudlet access key signature")
	}
	return nil
}

// Verify an access key signature in the grpc metadata
func (s *AccessKeyServer) VerifyAccessKeySig(ctx context.Context, method string) (*AccessKeyVerified, error) {
	// grab CloudletKey and signature from grpc metadata
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, fmt.Errorf("no meta data on grpc context")
	}
	data, found := md[cloudcommon.AccessKeyData]
	if !found || len(data) == 0 {
		return nil, fmt.Errorf("error, %s not found in metadata", cloudcommon.AccessKeyData)
	}
	verified := &AccessKeyVerified{}

	// data is the cloudlet key
	err := json.Unmarshal([]byte(data[0]), &verified.Key)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal cloudlet key from metadata, %s, %s", data, err)
	}
	// find public key to validate signature
	cloudlet := edgeproto.Cloudlet{}
	if !s.cloudletCache.Get(&verified.Key, &cloudlet) {
		return nil, fmt.Errorf("failed to find cloudlet %s to verify access key", data)
	}

	if cloudlet.EdgeboxOnly && method == GetAccessDataMethod {
		return nil, fmt.Errorf("Not allowed to get access data for Edgebox cloudlet")
	}

	// look up key signature
	sigb64, found := md[cloudcommon.AccessKeySig]
	if found && len(sigb64) > 0 {
		// access key signature
		sig, err := base64.StdEncoding.DecodeString(sigb64[0])
		if err != nil {
			return nil, fmt.Errorf("failed to base64 decode access key signature, %v", err)
		}

		if cloudlet.CrmAccessPublicKey == "" && cloudlet.SecondaryCrmAccessPublicKey == "" {
			return nil, fmt.Errorf("No crm access public key registered for cloudlet %s", data)
		}
		upgradeRequired := cloudlet.CrmAccessKeyUpgradeRequired
		upgradeMethod := UpgradeAccessKeyMethod
		err = s.verifyPublicKey(ctx, cloudlet.CrmAccessPublicKey, data[0], sig)
		if err != nil {
			if cloudlet.SecondaryCrmAccessPublicKey != "" {
				log.SpanLog(ctx, log.DebugLevelApi, "failed to verify primary access key, try secondary", "err", err)
				err = s.verifyPublicKey(ctx, cloudlet.SecondaryCrmAccessPublicKey, data[0], sig)
				upgradeRequired = cloudlet.SecondaryCrmAccessKeyUpgradeRequired
			}
		}
		if err != nil {
			return nil, err
		}

		log.SpanLog(ctx, log.DebugLevelApi, "verified access key", "CloudletKey", verified.Key)
		if upgradeRequired && method != upgradeMethod {
			return nil, fmt.Errorf("access key requires upgrade, does not allow api call %s", method)
		}
		verified.UpgradeRequired = upgradeRequired
		return verified, nil
	}
	vaultSig, found := md[cloudcommon.VaultKeySig]
	if found && len(vaultSig) > 0 {
		// vault key signature - only allowed for UpgradeAccessKey
		if method != UpgradeAccessKeyMethod {
			return nil, fmt.Errorf("vault auth not allowed for api %s", method)
		}
		verified.UpgradeRequired = true

		idkey := strings.Split(vaultSig[0], ",")
		if len(idkey) != 2 {
			return nil, fmt.Errorf("Vault signature format error, expected id,key but has %d fields", len(idkey))
		}
		// to authenticate, try to log into Vault
		vaultConfig := vault.NewAppRoleConfig(s.vaultAddr, idkey[0], idkey[1])
		_, err := vaultConfig.Login()
		if err != nil {
			log.SpanLog(ctx, log.DebugLevelApi, "failed to verify vault credentials", "err", err)
			return nil, fmt.Errorf("Failed to verify Vault credentials")
		}
		log.SpanLog(ctx, log.DebugLevelApi, "verified vault keys", "CloudletKey", verified.Key)
		return verified, nil
	}
	return nil, fmt.Errorf("no valid auth found")
}

// Grpc unary interceptor to require and verify access key
func (s *AccessKeyServer) UnaryRequireAccessKey(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	log.SpanLog(ctx, log.DebugLevelApi, "unary requiring access key")
	verified, err := s.VerifyAccessKeySig(ctx, info.FullMethod)
	if err != nil {
		// We intentionally do not return detailed errors, to avoid leaking of
		// information to malicious attackers, much like a usual "login"
		// function behaves.
		log.SpanLog(ctx, log.DebugLevelApi, "accesskey auth failed", "err", err)
		time.Sleep(BadAuthDelay)
		return nil, err
	}
	ctx = ContextSetAccessKeyVerified(ctx, verified)
	return handler(ctx, req)
}

// Grpc stream interceptor to require and verify access key
func (s *AccessKeyServer) StreamRequireAccessKey(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	ctx := stream.Context()
	log.SpanLog(ctx, log.DebugLevelApi, "stream requiring access key")
	verified, err := s.VerifyAccessKeySig(ctx, info.FullMethod)
	if err != nil {
		log.SpanLog(ctx, log.DebugLevelApi, "accesskey auth failed", "err", err)
		time.Sleep(BadAuthDelay)
		return err
	}
	ctx = ContextSetAccessKeyVerified(ctx, verified)
	// override context on existing stream, since no way to set it
	stream = cloudcommon.WrapStream(stream, ctx)
	return handler(srv, stream)

}

// Grpc unary interceptor to require and verify access key based on client cert
func (s *AccessKeyServer) UnaryTlsAccessKey(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	required, err := s.isTlsAccessKeyRequired(ctx)
	if err != nil {
		return nil, err
	}
	if required {
		return s.UnaryRequireAccessKey(ctx, req, info, handler)
	}
	return handler(ctx, req)
}

// Grpc stream interceptor to require and verify access key based on client cert
func (s *AccessKeyServer) StreamTlsAccessKey(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	required, err := s.isTlsAccessKeyRequired(stream.Context())
	if err != nil {
		return err
	}
	if required {
		return s.StreamRequireAccessKey(srv, stream, info, handler)
	}
	return handler(srv, stream)
}

// Determines from the grpc context if an access key is required.
func (s *AccessKeyServer) isTlsAccessKeyRequired(ctx context.Context) (bool, error) {
	if !s.requireTlsAccessKey {
		return false, nil
	}
	pr, ok := peer.FromContext(ctx)
	if !ok {
		return false, fmt.Errorf("no grpc peer context")
	}
	tlsInfo, ok := pr.AuthInfo.(credentials.TLSInfo)
	if ok {
		for _, chain := range tlsInfo.State.VerifiedChains {
			for _, cert := range chain {
				if !cert.IsCA || len(cert.DNSNames) == 0 {
					continue
				}
				commonName := cert.DNSNames[0]
				// if cert is issued by regional-access-key,
				// then access key verification is required.
				if commonName == CertIssuerRegionalCloudlet {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func (s *AccessKeyServer) UpgradeAccessKey(stream edgeproto.CloudletAccessKeyApi_UpgradeAccessKeyServer, commitKeyFunc func(ctx context.Context, key *edgeproto.CloudletKey, pubPEM string, role process.HARole) error) error {
	ctx := stream.Context()
	verified := ContextGetAccessKeyVerified(ctx)
	haRole := process.HARolePrimary
	if verified == nil {
		// this should never happen, the interceptor should error out first
		return fmt.Errorf("access key not verified")
	}
	msg, err := stream.Recv()
	if err != nil {
		return err
	}
	if msg.VerifyOnly {
		log.SpanLog(ctx, log.DebugLevelApi, "access key verifyOnly")
		// Non-CRM service that is verifying the access key.
		// Fail verification if upgrade is required.
		if verified.UpgradeRequired {
			return fmt.Errorf("access key upgrade required")
		}
		return stream.Send(&edgeproto.UpgradeAccessKeyServerMsg{
			Msg: "verified",
		})
	}
	if msg.HaRole != "" {
		haRole = process.HARole(msg.HaRole)
	}

	if !verified.UpgradeRequired {
		log.SpanLog(ctx, log.DebugLevelApi, "access key upgrade not required")
		return stream.Send(&edgeproto.UpgradeAccessKeyServerMsg{
			Msg: "upgrade-not-needed",
		})
	}
	log.SpanLog(ctx, log.DebugLevelApi, "generating new access key")
	// upgrade required, generate new key
	keyPair, err := GenerateAccessKey()
	if err != nil {
		return err
	}
	log.SpanLog(ctx, log.DebugLevelApi, "sending new access key")
	err = stream.Send(&edgeproto.UpgradeAccessKeyServerMsg{
		Msg:                 "new-key",
		CrmPrivateAccessKey: keyPair.PrivatePEM,
	})
	if err != nil {
		return err
	}
	log.SpanLog(ctx, log.DebugLevelApi, "waiting for ack")
	// Read ack to make sure CRM got new key.
	// See comments in client code for UpgradeAccessKey for error recovery.
	_, err = stream.Recv()
	if err != nil {
		return err
	}
	log.SpanLog(ctx, log.DebugLevelApi, "ack received, committing new key")
	err = commitKeyFunc(ctx, &verified.Key, keyPair.PublicPEM, haRole)
	if err != nil {
		return err
	}
	// this final send makes client wait until new public key pem
	// has been committed, otherwise client will try to connect
	// immediately with new private key before new public key has
	// been put into cache by etcd watch.
	return stream.Send(&edgeproto.UpgradeAccessKeyServerMsg{
		Msg: "commit-complete",
	})
}

// AccessKeyGrcpServer starts up a grpc listener for the access API endpoint.
// This is used both by the Controller and various unit test code, and keeps
// the interceptor setup consistent while avoiding duplicate code.
type AccessKeyGrpcServer struct {
	lis             net.Listener
	serv            *grpc.Server
	AccessKeyServer *AccessKeyServer
}

func (s *AccessKeyGrpcServer) Start(addr string, keyServer *AccessKeyServer, tlsConfig *tls.Config, registerHandlers func(server *grpc.Server)) error {
	s.AccessKeyServer = keyServer

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.lis = lis

	// start AccessKey grpc service.
	grpcServer := grpc.NewServer(cloudcommon.GrpcCreds(tlsConfig),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			cloudcommon.AuditUnaryInterceptor,
			s.AccessKeyServer.UnaryRequireAccessKey,
		)),
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
			cloudcommon.AuditStreamInterceptor,
			s.AccessKeyServer.StreamRequireAccessKey,
		)),
		grpc.KeepaliveParams(cloudcommon.GRPCServerKeepaliveParams),
		grpc.KeepaliveEnforcementPolicy(cloudcommon.GRPCServerKeepaliveEnforcement),
		grpc.ForceServerCodec(&cloudcommon.ProtoCodec{}),
	)
	if registerHandlers != nil {
		registerHandlers(grpcServer)
	}

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			log.FatalLog("Failed to serve", "err", err)
		}
	}()
	s.serv = grpcServer
	return nil
}

func (s *AccessKeyGrpcServer) Stop() {
	if s.serv != nil {
		s.serv.Stop()
		s.serv = nil
	}
	if s.lis != nil {
		s.lis.Close()
		s.lis = nil
	}
}

func (s *AccessKeyGrpcServer) ApiAddr() string {
	return s.lis.Addr().String()
}

// Basic edgeproto.CloudletAccessKeyApiServer handler for unit tests only.
type BasicUpgradeHandler struct {
	KeyServer *AccessKeyServer
}

func (s *BasicUpgradeHandler) UpgradeAccessKey(stream edgeproto.CloudletAccessKeyApi_UpgradeAccessKeyServer) error {
	return s.KeyServer.UpgradeAccessKey(stream, s.commitKey)
}

func (s *BasicUpgradeHandler) commitKey(ctx context.Context, key *edgeproto.CloudletKey, pubPEM string, role process.HARole) error {
	// Not thread safe, unit-test only.
	cloudlet := &edgeproto.Cloudlet{}
	if !s.KeyServer.cloudletCache.Get(key, cloudlet) {
		return key.NotFoundError()
	}
	if role == process.HARoleSecondary {
		cloudlet.SecondaryCrmAccessPublicKey = pubPEM
		cloudlet.SecondaryCrmAccessKeyUpgradeRequired = false
	} else {
		cloudlet.CrmAccessPublicKey = pubPEM
		cloudlet.CrmAccessKeyUpgradeRequired = false
	}
	s.KeyServer.cloudletCache.Update(ctx, cloudlet, 0)
	return nil
}
