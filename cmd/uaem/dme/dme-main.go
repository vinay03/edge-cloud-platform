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
	"context"
	ctls "crypto/tls"
	"flag"
	"fmt"
	"io/ioutil"
	baselog "log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	dme "github.com/edgexr/edge-cloud-platform/api/distributed_match_engine"
	"github.com/edgexr/edge-cloud-platform/api/edgeproto"
	"github.com/edgexr/edge-cloud-platform/pkg/accessapi"
	accessapicloudlet "github.com/edgexr/edge-cloud-platform/pkg/accessapi-cloudlet"
	"github.com/edgexr/edge-cloud-platform/pkg/cloudcommon"
	"github.com/edgexr/edge-cloud-platform/pkg/cloudcommon/ratelimit"
	"github.com/edgexr/edge-cloud-platform/pkg/cloudcommon/svcnode"
	log "github.com/edgexr/edge-cloud-platform/pkg/log"
	"github.com/edgexr/edge-cloud-platform/pkg/notify"
	op "github.com/edgexr/edge-cloud-platform/pkg/nrem-platform"
	operator "github.com/edgexr/edge-cloud-platform/pkg/nrem-platform"
	"github.com/edgexr/edge-cloud-platform/pkg/nrem-platform/defaultoperator"
	"github.com/edgexr/edge-cloud-platform/pkg/platform"
	"github.com/edgexr/edge-cloud-platform/pkg/plugin/edgeevents"
	pplat "github.com/edgexr/edge-cloud-platform/pkg/plugin/platform"
	"github.com/edgexr/edge-cloud-platform/pkg/tls"
	uaemcommon "github.com/edgexr/edge-cloud-platform/pkg/uaem-common"
	"github.com/edgexr/edge-cloud-platform/pkg/util"
	"github.com/edgexr/edge-cloud-platform/pkg/vault"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	gwruntime "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/segmentio/ksuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// Command line options
var rootDir = flag.String("r", "", "root directory for testing")
var notifyAddrs = flag.String("notifyAddrs", "127.0.0.1:50001", "Comma separated list of controller notify listener addresses")
var apiAddr = flag.String("apiAddr", "localhost:50051", "API listener address")
var httpAddr = flag.String("httpAddr", "127.0.0.1:38001", "HTTP listener address")
var debugLevels = flag.String("d", "", fmt.Sprintf("comma separated list of %v", log.DebugLevelStrings))
var locVerUrl = flag.String("locverurl", "", "location verification REST API URL to connect to")
var tokSrvUrl = flag.String("toksrvurl", "", "token service URL to provide to client on register")
var qosPosUrl = flag.String("qosposurl", "", "QOS Position KPI URL to connect to")
var qosSesAddr = flag.String("qossesaddr", "", "QOS for stable bandwidth address to connect to")
var tlsApiCertFile = flag.String("tlsApiCertFile", "", "Public-CA signed TLS cert file for serving DME APIs")
var tlsApiKeyFile = flag.String("tlsApiKeyFile", "", "Public-CA signed TLS key file for serving DME APIs")
var cloudletKeyStr = flag.String("cloudletKey", "", "Json or Yaml formatted cloudletKey for the cloudlet in which this CRM is instantiated; e.g. '{\"operator_key\":{\"name\":\"DMUUS\"},\"name\":\"tmocloud1\"}'")
var statsShards = flag.Uint("statsShards", 10, "number of shards (locks) in memory for parallel stat collection")
var cookieExpiration = flag.Duration("cookieExpiration", time.Hour*24, "Cookie expiration time")
var edgeEventsCookieExpiration = flag.Duration("edgeEventsCookieExpiration", time.Minute*10, "Edge Events Cookie expiration time")
var region = flag.String("region", "local", "region name")
var solib = flag.String("plugin", "", "plugin file")
var eesolib = flag.String("eeplugin", "", "plugin file") // for edge events plugin
var testMode = flag.Bool("testMode", false, "Run controller in test mode")
var cloudletDme = flag.Bool("cloudletDme", false, "this is a cloudlet DME deployed on cloudlet infrastructure and uses the crm access key")

// TODO: carrier arg is redundant with Organization in MyCloudletKey, and
// should be replaced by it, but requires dealing with carrier-specific
// verify location API behavior and e2e test setups.
var carrier = flag.String("carrier", "standalone", "carrier name for API connection, or standalone for no external APIs")

var operatorApiGw op.OperatorApiGw

// server is used to implement helloworld.GreeterServer.
type server struct{}

var nodeMgr svcnode.SvcNodeMgr

var sigChan chan os.Signal

func (s *server) FindCloudlet(ctx context.Context, req *dme.FindCloudletRequest) (*dme.FindCloudletReply, error) {
	reply := new(dme.FindCloudletReply)
	var appkey edgeproto.AppKey
	var app *uaemcommon.DmeApp
	ckey, ok := uaemcommon.CookieFromContext(ctx)
	if !ok {
		return reply, grpc.Errorf(codes.InvalidArgument, "No valid session cookie")
	}
	appkey.Organization = ckey.OrgName
	appkey.Name = ckey.AppName
	appkey.Version = ckey.AppVers

	err := uaemcommon.ValidateLocation(req.GpsLocation)
	if err != nil {
		log.SpanLog(ctx, log.DebugLevelDmereq, "Invalid FindCloudlet request, invalid location", "loc", req.GpsLocation, "err", err)
		return reply, err
	}

	log.SpanLog(ctx, log.DebugLevelDmereq, "req tags", "req.Tags", req.Tags)
	err, app = uaemcommon.FindCloudlet(ctx, &appkey, req.CarrierName, req.GpsLocation, reply, *edgeEventsCookieExpiration)

	// Only attempt to create a QOS priority session if qosSesAddr is populated.
	if *qosSesAddr != "" && err == nil && reply.Status == dme.FindCloudletReply_FIND_FOUND {
		app.Lock()
		defer app.Unlock()
		log.SpanLog(ctx, log.DebugLevelDmereq, "FindCloudlet returned app", "QosSessionProfile", app.QosSessionProfile, "QosSessionDuration", app.QosSessionDuration)
		qos := app.QosSessionProfile
		duration := app.QosSessionDuration
		if duration == 0 {
			duration = 24 * time.Hour // 24 hours - default value
		}
		log.SpanLog(ctx, log.DebugLevelDmereq, "Session duration", "app.QosSessionDuration", app.QosSessionDuration, " derived duration", duration)

		if qos != "DEFAULT" {
			var protocol string
			var asAddr string
			var ips []net.IP
			if os.Getenv("E2ETEST_TLS") != "" {
				// avoid IP lookup, it hangs and causes API call to timeout
				log.SpanLog(ctx, log.DebugLevelDmereq, "Avoid Ip lookup for e2e test")
			} else {
				ips, _ = net.LookupIP(reply.Fqdn)
			}
			for _, ip := range ips {
				if ipv4 := ip.To4(); ipv4 != nil {
					log.SpanLog(ctx, log.DebugLevelDmereq, "Looked up IPv4 address", "reply.Fqdn", reply.Fqdn, "ipv4", ipv4)
					asAddr = ipv4.String()
					break
				}
			}
			if asAddr == "" && os.Getenv("E2ETEST_QOS_SIM") == "true" {
				// If running e2e-test, the sessions-srv-sim will be used where the asAddr is ignored.
				// FQDN lookup above failed, so just set it to any known good IP (in this case, localhost).
				asAddr = "127.0.0.1"
				log.SpanLog(ctx, log.DebugLevelDmereq, "Running e2e-test. Setting asAddr to localhost", "asAddr", asAddr)
			}

			ueAddr := req.Tags[cloudcommon.TagIpUserEquipment]
			// Use the first port
			port := app.Ports[0]
			log.SpanLog(ctx, log.DebugLevelDmereq, "Port", "port.PublicPort", port.PublicPort, "port.Proto", port.Proto, "port.InternalPort", port.InternalPort)
			if port.Proto == dme.LProto_L_PROTO_TCP {
				protocol = "TCP"
			} else if port.Proto == dme.LProto_L_PROTO_UDP {
				protocol = "UDP"
			} else if port.Proto == dme.LProto_L_PROTO_HTTP {
				protocol = "HTTP"
			} else {
				protocol = ""
			}
			asPort := fmt.Sprintf("%d", port.InternalPort)

			if qos == "QOS_NO_PRIORITY" {
				msg := "QOS_NO_PRIORITY specified. Will not create priority session"
				log.SpanLog(ctx, log.DebugLevelDmereq, msg)
				reply.QosResult = dme.FindCloudletReply_QOS_NOT_ATTEMPTED
			} else if ueAddr == "" {
				msg := "ip_user_equipment value not found in tags"
				log.SpanLog(ctx, log.DebugLevelDmereq, msg, "req.Tags", req.Tags)
				reply.QosResult = dme.FindCloudletReply_QOS_SESSION_FAILED
				reply.QosErrorMsg = msg
			} else if asAddr == "" {
				msg := "Could not decode app inst FQDN"
				log.SpanLog(ctx, log.DebugLevelDmereq, msg, "reply.Fqdn", reply.Fqdn)
				reply.QosResult = dme.FindCloudletReply_QOS_SESSION_FAILED
				reply.QosErrorMsg = msg
			} else if protocol == "" {
				msg := "Unknown port protocol."
				log.SpanLog(ctx, log.DebugLevelDmereq, msg, "port.Proto", port.Proto)
				reply.QosResult = dme.FindCloudletReply_QOS_SESSION_FAILED
				reply.QosErrorMsg = msg
			} else {
				// Build a new QosPrioritySessionCreateRequest and populate fields.
				qosReq := new(dme.QosPrioritySessionCreateRequest)
				qosReq.IpApplicationServer = asAddr
				qosReq.IpUserEquipment = ueAddr
				qosReq.PortApplicationServer = asPort
				qosReq.Profile, _ = dme.ParseQosSessionProfile(qos)
				qosReq.ProtocolIn, _ = dme.ParseQosSessionProtocol(protocol)
				qosReq.SessionDuration = uint32(duration.Seconds())
				log.SpanLog(ctx, log.DebugLevelDmereq, "Built new qosReq", "qosReq", qosReq)
				sesReply, sesErr := operatorApiGw.CreatePrioritySession(ctx, qosReq)
				if sesErr != nil {
					log.SpanLog(ctx, log.DebugLevelDmereq, "CreatePrioritySession failed.", "sesErr", sesErr)
					reply.QosResult = dme.FindCloudletReply_QOS_SESSION_FAILED
					reply.QosErrorMsg = sesErr.Error()
				} else {
					sesReplyToLog := *sesReply // Copy so we can redact session Id in log
					sesReplyToLog.SessionId = "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
					log.SpanLog(ctx, log.DebugLevelDmereq, "CreatePrioritySession() returned", "sesReply", sesReplyToLog)

					// Let the client know the session ID.
					reply.Tags = make(map[string]string)
					if sesReply.SessionId != "" {
						reply.Tags[cloudcommon.TagPrioritySessionId] = sesReply.SessionId
						reply.Tags[cloudcommon.TagQosProfileName] = qos
						reply.QosResult = dme.FindCloudletReply_QOS_SESSION_CREATED
					} else {
						msg := "No session ID received from QOS API server"
						log.SpanLog(ctx, log.DebugLevelDmereq, msg)
						reply.QosResult = dme.FindCloudletReply_QOS_SESSION_FAILED
						reply.QosErrorMsg = msg
					}
				}
			}
		}
	} else {
		reply.QosResult = dme.FindCloudletReply_QOS_NOT_ATTEMPTED
	}

	log.SpanLog(ctx, log.DebugLevelDmereq, "FindCloudlet returns", "reply", reply, "error", err)
	return reply, err
}

func (s *server) PlatformFindCloudlet(ctx context.Context, req *dme.PlatformFindCloudletRequest) (*dme.FindCloudletReply, error) {
	reply := new(dme.FindCloudletReply)
	ckey, ok := uaemcommon.CookieFromContext(ctx)
	if !ok {
		return reply, grpc.Errorf(codes.InvalidArgument, "No valid session cookie")
	}
	if !cloudcommon.IsPlatformApp(ckey.OrgName, ckey.AppName) {
		log.SpanLog(ctx, log.DebugLevelDmereq, "PlatformFindCloudlet API Not allowed for developer app", "org", ckey.OrgName, "name", ckey.AppName)
		return reply, grpc.Errorf(codes.PermissionDenied, "API Not allowed for developer: %s app: %s", ckey.OrgName, ckey.AppName)
	}
	if req.ClientToken == "" {
		log.SpanLog(ctx, log.DebugLevelDmereq, "Invalid PlatformFindCloudlet request", "Error", "Missing ClientToken")
		return reply, grpc.Errorf(codes.InvalidArgument, "Missing ClientToken")
	}
	tokdata, err := uaemcommon.GetClientDataFromToken(req.ClientToken)
	if err != nil {
		log.SpanLog(ctx, log.DebugLevelDmereq, "Invalid PlatformFindCloudletRequest request, unable to get data from token", "token", req.ClientToken, "err", err)
		return reply, grpc.Errorf(codes.InvalidArgument, "Invalid ClientToken")
	}

	if tokdata.AppKey.Organization == "" {
		log.SpanLog(ctx, log.DebugLevelDmereq, "OrgName in token cannot be empty")
		return reply, grpc.Errorf(codes.InvalidArgument, "OrgName in token cannot be empty")
	}
	if tokdata.AppKey.Name == "" {
		log.SpanLog(ctx, log.DebugLevelDmereq, "AppName in token cannot be empty")
		return reply, grpc.Errorf(codes.InvalidArgument, "AppName in token cannot be empty")
	}
	if tokdata.AppKey.Version == "" {
		log.SpanLog(ctx, log.DebugLevelDmereq, "AppVers in token cannot be empty")
		return reply, grpc.Errorf(codes.InvalidArgument, "AppVers in token cannot be empty")
	}
	if !uaemcommon.AppExists(tokdata.AppKey.Organization, tokdata.AppKey.Name, tokdata.AppKey.Version) {
		log.SpanLog(ctx, log.DebugLevelDmereq, "Requested app does not exist", "requestedAppKey", tokdata.AppKey)
		return reply, grpc.Errorf(codes.InvalidArgument, "Requested app does not exist")
	}
	err = uaemcommon.ValidateLocation(&tokdata.Location)
	if err != nil {
		log.SpanLog(ctx, log.DebugLevelDmereq, "Invalid PlatformFindCloudletRequest request, invalid location", "loc", tokdata.Location, "err", err)
		return reply, grpc.Errorf(codes.InvalidArgument, "Invalid ClientToken")
	}
	err, _ = uaemcommon.FindCloudlet(ctx, &tokdata.AppKey, req.CarrierName, &tokdata.Location, reply, *edgeEventsCookieExpiration)
	log.SpanLog(ctx, log.DebugLevelDmereq, "PlatformFindCloudletRequest returns", "reply", reply, "error", err)
	return reply, err
}

func (s *server) QosPrioritySessionCreate(ctx context.Context, req *dme.QosPrioritySessionCreateRequest) (*dme.QosPrioritySessionReply, error) {
	log.SpanLog(ctx, log.DebugLevelDmereq, "QosPrioritySessionCreate", "req", req)
	reply := new(dme.QosPrioritySessionReply)
	var appkey edgeproto.AppKey
	ckey, ok := uaemcommon.CookieFromContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "No valid session cookie")
	}
	appkey.Organization = ckey.OrgName
	appkey.Name = ckey.AppName
	appkey.Version = ckey.AppVers
	log.SpanLog(ctx, log.DebugLevelDmereq, "appkey", "appkey", appkey, "appkey.Name", appkey.Name)
	if *qosSesAddr != "" {
		log.SpanLog(ctx, log.DebugLevelDmereq, "qosSesAddr defined", "qosSesAddr", qosSesAddr, "req", req)
		sesReply, sesErr := operatorApiGw.CreatePrioritySession(ctx, req)
		if sesErr != nil {
			log.SpanLog(ctx, log.DebugLevelDmereq, "CreatePrioritySession failed.", "sesErr", sesErr)
			return nil, sesErr
		}
		reply = sesReply
		sesReplyToLog := *sesReply // Copy so we can redact session Id in log
		sesReplyToLog.SessionId = "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
		log.SpanLog(ctx, log.DebugLevelDmereq, "operatorApiGw.CreatePrioritySession() returned", "sesReply", sesReplyToLog, "sesErr", sesErr)
	} else {
		return nil, status.Errorf(codes.InvalidArgument, "Cannot create session because qosSesAddr not defined.")
	}
	return reply, nil
}

func (s *server) QosPrioritySessionDelete(ctx context.Context, req *dme.QosPrioritySessionDeleteRequest) (*dme.QosPrioritySessionDeleteReply, error) {
	log.SpanLog(ctx, log.DebugLevelDmereq, "QosPrioritySessionDelete", "req", req)
	reply := new(dme.QosPrioritySessionDeleteReply)
	var appkey edgeproto.AppKey
	ckey, ok := uaemcommon.CookieFromContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "No valid session cookie")
	}
	appkey.Organization = ckey.OrgName
	appkey.Name = ckey.AppName
	appkey.Version = ckey.AppVers
	log.SpanLog(ctx, log.DebugLevelDmereq, "appkey", "appkey", appkey, "appkey.Name", appkey.Name)
	if *qosSesAddr != "" {
		log.SpanLog(ctx, log.DebugLevelDmereq, "qosSesAddr defined", "qosSesAddr", qosSesAddr, "req", req)
		sesId := req.SessionId
		log.SpanLog(ctx, log.DebugLevelDmereq, "QOS Priority Session will be deleted", "sesId", sesId)
		sesReply, sesErr := operatorApiGw.DeletePrioritySession(ctx, req)
		if sesErr != nil {
			log.SpanLog(ctx, log.DebugLevelDmereq, "DeletePrioritySession failed.", "sesErr", sesErr)
			return nil, sesErr
		}
		log.SpanLog(ctx, log.DebugLevelDmereq, "operatorApiGw.DeletePrioritySession() successful")
		reply = sesReply
	}
	return reply, nil
}

func (s *server) GetFqdnList(ctx context.Context, req *dme.FqdnListRequest) (*dme.FqdnListReply, error) {
	log.SpanLog(ctx, log.DebugLevelDmereq, "GetFqdnList", "req", req)
	flist := new(dme.FqdnListReply)

	ckey, ok := uaemcommon.CookieFromContext(ctx)
	if !ok {
		return nil, grpc.Errorf(codes.InvalidArgument, "No valid session cookie")
	}
	// normal applications are not allowed to access this, only special platform developer/app combos
	if !cloudcommon.IsPlatformApp(ckey.OrgName, ckey.AppName) {
		return nil, grpc.Errorf(codes.PermissionDenied, "API Not allowed for developer: %s app: %s", ckey.OrgName, ckey.AppName)
	}

	uaemcommon.GetFqdnList(req, flist)
	log.SpanLog(ctx, log.DebugLevelDmereq, "GetFqdnList returns", "status", flist.Status)
	return flist, nil
}

func (s *server) GetAppInstList(ctx context.Context, req *dme.AppInstListRequest) (*dme.AppInstListReply, error) {
	ckey, ok := uaemcommon.CookieFromContext(ctx)
	if !ok {
		return nil, grpc.Errorf(codes.InvalidArgument, "No valid session cookie")
	}

	log.SpanLog(ctx, log.DebugLevelDmereq, "GetAppInstList", "carrier", req.CarrierName, "ckey", ckey)

	if req.GpsLocation == nil {
		log.SpanLog(ctx, log.DebugLevelDmereq, "Invalid GetAppInstList request", "Error", "Missing GpsLocation")
		return nil, grpc.Errorf(codes.InvalidArgument, "Missing GPS location")
	}
	alist := new(dme.AppInstListReply)
	uaemcommon.GetAppInstList(ctx, ckey, req, alist, *edgeEventsCookieExpiration)
	log.SpanLog(ctx, log.DebugLevelDmereq, "GetAppInstList returns", "status", alist.Status)
	return alist, nil
}

func (s *server) GetAppOfficialFqdn(ctx context.Context, req *dme.AppOfficialFqdnRequest) (*dme.AppOfficialFqdnReply, error) {
	ckey, ok := uaemcommon.CookieFromContext(ctx)
	reply := new(dme.AppOfficialFqdnReply)
	if !ok {
		return nil, grpc.Errorf(codes.InvalidArgument, "No valid session cookie")
	}
	log.SpanLog(ctx, log.DebugLevelDmereq, "GetAppOfficialFqdn", "ckey", ckey, "loc", req.GpsLocation)
	err := uaemcommon.ValidateLocation(req.GpsLocation)
	if err != nil {
		log.SpanLog(ctx, log.DebugLevelDmereq, "Invalid GetAppOfficialFqdn request, invalid location", "loc", req.GpsLocation, "err", err)
		return reply, err
	}
	uaemcommon.GetAppOfficialFqdn(ctx, ckey, req, reply)
	log.SpanLog(ctx, log.DebugLevelDmereq, "GetAppOfficialFqdn returns", "status", reply.Status)
	return reply, nil
}

func (s *server) VerifyLocation(ctx context.Context,
	req *dme.VerifyLocationRequest) (*dme.VerifyLocationReply, error) {

	reply := new(dme.VerifyLocationReply)

	reply.GpsLocationStatus = dme.VerifyLocationReply_LOC_UNKNOWN
	reply.GpsLocationAccuracyKm = -1

	log.SpanLog(ctx, log.DebugLevelDmereq, "Received Verify Location",
		"VerifyLocToken", req.VerifyLocToken,
		"GpsLocation", req.GpsLocation)

	if req.GpsLocation == nil || (req.GpsLocation.Latitude == 0 && req.GpsLocation.Longitude == 0) {
		log.SpanLog(ctx, log.DebugLevelDmereq, "Invalid VerifyLocation request", "Error", "Missing GpsLocation")
		return reply, grpc.Errorf(codes.InvalidArgument, "Missing GPS location")
	}

	if !util.IsLatitudeValid(req.GpsLocation.Latitude) || !util.IsLongitudeValid(req.GpsLocation.Longitude) {
		log.SpanLog(ctx, log.DebugLevelDmereq, "Invalid VerifyLocation GpsLocation", "lat", req.GpsLocation.Latitude, "long", req.GpsLocation.Longitude)
		return reply, grpc.Errorf(codes.InvalidArgument, "Invalid GpsLocation")
	}
	err := operatorApiGw.VerifyLocation(req, reply)
	return reply, err

}

func (s *server) GetLocation(ctx context.Context,
	req *dme.GetLocationRequest) (*dme.GetLocationReply, error) {
	reply := new(dme.GetLocationReply)
	err := operatorApiGw.GetLocation(req, reply)
	return reply, err
}

func (s *server) RegisterClient(ctx context.Context,
	req *dme.RegisterClientRequest) (*dme.RegisterClientReply, error) {

	mstatus := new(dme.RegisterClientReply)

	log.SpanLog(ctx, log.DebugLevelDmereq, "RegisterClient received", "request", req)

	if req.OrgName == "" {
		log.SpanLog(ctx, log.DebugLevelDmereq, "OrgName cannot be empty")
		mstatus.Status = dme.ReplyStatus_RS_FAIL
		return mstatus, grpc.Errorf(codes.InvalidArgument, "OrgName cannot be empty")
	}
	if req.AppName == "" {
		log.SpanLog(ctx, log.DebugLevelDmereq, "AppName cannot be empty")
		mstatus.Status = dme.ReplyStatus_RS_FAIL
		return mstatus, grpc.Errorf(codes.InvalidArgument, "AppName cannot be empty")
	}
	if req.AppVers == "" {
		log.SpanLog(ctx, log.DebugLevelDmereq, "AppVers cannot be empty")
		mstatus.Status = dme.ReplyStatus_RS_FAIL
		return mstatus, grpc.Errorf(codes.InvalidArgument, "AppVers cannot be empty")
	}
	authkey, err := uaemcommon.GetAuthPublicKey(req.OrgName, req.AppName, req.AppVers)
	if err != nil {
		log.SpanLog(ctx, log.DebugLevelDmereq, "fail to get public key", "err", err)
		mstatus.Status = dme.ReplyStatus_RS_FAIL
		return mstatus, err
	}

	//the token is currently optional, but once the SDK is enhanced to send one, it should
	// be a mandatory parameter.  For now, only validate the token if we receive one
	if req.AuthToken == "" {
		if authkey != "" {
			// we provisioned a key, and one was not provided.
			log.SpanLog(ctx, log.DebugLevelDmereq, "App has key, no token received")
			mstatus.Status = dme.ReplyStatus_RS_FAIL
			return mstatus, grpc.Errorf(codes.InvalidArgument, "No authtoken received")
		}
		// for now we will allow a tokenless register to pass if the app does not have one
		log.SpanLog(ctx, log.DebugLevelDmereq, "Allowing register without token")

	} else {
		if authkey == "" {
			log.SpanLog(ctx, log.DebugLevelDmereq, "No authkey provisioned to validate token")
			mstatus.Status = dme.ReplyStatus_RS_FAIL
			return mstatus, grpc.Errorf(codes.Unauthenticated, "No authkey found to validate token")
		}
		err := uaemcommon.VerifyAuthToken(ctx, req.AuthToken, authkey, req.OrgName, req.AppName, req.AppVers)
		if err != nil {
			log.SpanLog(ctx, log.DebugLevelDmereq, "Failed to verify token", "err", err)
			mstatus.Status = dme.ReplyStatus_RS_FAIL
			return mstatus, grpc.Errorf(codes.Unauthenticated, "failed to verify token - %s", err.Error())
		}
	}

	key := uaemcommon.CookieKey{
		OrgName: req.OrgName,
		AppName: req.AppName,
		AppVers: req.AppVers,
	}

	// Both UniqueId and UniqueIdType should be set, or none
	if (req.UniqueId != "" && req.UniqueIdType == "") ||
		(req.UniqueIdType != "" && req.UniqueId == "") {
		mstatus.Status = dme.ReplyStatus_RS_FAIL
		return mstatus, grpc.Errorf(codes.InvalidArgument,
			"Both, or none of UniqueId and UniqueIdType should be set")
	}

	// If we get UUID from the Request, use that, otherwise generate a new one
	if req.UniqueIdType != "" && req.UniqueId != "" {
		key.UniqueId = req.UniqueId
		key.UniqueIdType = req.UniqueIdType
	} else {
		// Generate KSUID
		uid := ksuid.New()
		key.UniqueId = uid.String()
		if cloudcommon.IsPlatformApp(req.OrgName, req.AppName) {
			key.UniqueIdType = req.OrgName + ":" + req.AppName
		} else {
			key.UniqueIdType = "dme-ksuid"
		}
	}

	cookie, err := uaemcommon.GenerateCookie(&key, ctx, cookieExpiration)
	if err != nil {
		return mstatus, grpc.Errorf(codes.Internal, err.Error())
	}

	// Set UUID in the reply if none were sent in the request
	// We only respond back with a UUID if it's generated by the DME
	if req.UniqueIdType == "" || req.UniqueId == "" {
		mstatus.UniqueIdType = key.UniqueIdType
		mstatus.UniqueId = key.UniqueId
	}
	mstatus.SessionCookie = cookie
	mstatus.TokenServerUri = *tokSrvUrl
	mstatus.Status = dme.ReplyStatus_RS_SUCCESS
	return mstatus, nil
}

func (s *server) AddUserToGroup(ctx context.Context,
	req *dme.DynamicLocGroupRequest) (*dme.DynamicLocGroupReply, error) {

	mreq := new(dme.DynamicLocGroupReply)
	mreq.Status = dme.ReplyStatus_RS_SUCCESS

	return mreq, nil
}

func (s *server) GetQosPositionKpi(req *dme.QosPositionRequest, getQosSvr dme.QosPositionKpi_GetQosPositionKpiServer) error {
	log.SpanLog(getQosSvr.Context(), log.DebugLevelDmereq, "GetQosPositionKpi", "request", req)
	return operatorApiGw.GetQOSPositionKPI(req, getQosSvr)
}

func (s *server) StreamEdgeEvent(streamEdgeEventSvr dme.MatchEngineApi_StreamEdgeEventServer) error {
	ctx := streamEdgeEventSvr.Context()
	log.SpanLog(ctx, log.DebugLevelDmereq, "StreamEdgeEvent")
	return uaemcommon.StreamEdgeEvent(ctx, streamEdgeEventSvr, *edgeEventsCookieExpiration)
}

func initOperator(ctx context.Context, operatorName string) (op.OperatorApiGw, error) {
	if operatorName == "" || operatorName == "standalone" {
		return &defaultoperator.OperatorApiGw{}, nil
	}
	apiGw, err := pplat.GetOperatorApiGw(ctx, operatorName)
	if err != nil {
		return nil, err
	}
	nodeMgr.UpdateNodeProps(ctx, apiGw.GetVersionProperties(ctx))
	return apiGw, nil
}

// Loads EdgeEvent Plugin functions into EEHandler
func initEdgeEventsPlugin(ctx context.Context, operatorName string) (uaemcommon.EdgeEventsHandler, error) {
	if operatorName == "" || operatorName == "standalone" {
		return &uaemcommon.EmptyEdgeEventsHandler{}, nil
	}
	eehandler, err := edgeevents.GetEdgeEventsHandler(ctx, *edgeEventsCookieExpiration)
	if err != nil {
		return nil, err
	}
	nodeMgr.UpdateNodeProps(ctx, eehandler.GetVersionProperties(ctx))
	return eehandler, nil
}

// allowCORS allows Cross Origin Resoruce Sharing from any origin.
func allowCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			if r.Method == "OPTIONS" && r.Header.Get("Access-Control-Request-Method") != "" {
				// preflight headers
				headers := []string{"Content-Type", "Accept"}
				w.Header().Set("Access-Control-Allow-Headers", strings.Join(headers, ","))
				methods := []string{"GET", "HEAD", "POST", "PUT", "DELETE"}
				w.Header().Set("Access-Control-Allow-Methods", strings.Join(methods, ","))
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// Helper function that creates all the RateLimitSettings for the specified API, updates cache, and then adds to RateLimitMgr
func addDmeApiRateLimitSettings(ctx context.Context, apiName string) {
	settingsMap := edgeproto.GetDefaultRateLimitSettings()
	// Get RateLimitSettings that correspond to the key
	allRequestsRateLimitSettings := getDmeApiRateLimitSettings(apiName, edgeproto.RateLimitTarget_ALL_REQUESTS, settingsMap)
	perIpRateLimitSettings := getDmeApiRateLimitSettings(apiName, edgeproto.RateLimitTarget_PER_IP, settingsMap)
	perUserRateLimitSettings := getDmeApiRateLimitSettings(apiName, edgeproto.RateLimitTarget_PER_USER, settingsMap)
	// Add apiendpoint limiter to RateLimitMgrs
	uaemcommon.RateLimitMgr.CreateApiEndpointLimiter(apiName, allRequestsRateLimitSettings, perIpRateLimitSettings, perUserRateLimitSettings)
}

// Helper function that generates a RateLimitSettings struct for specified api and target
func getDmeApiRateLimitSettings(apiName string, target edgeproto.RateLimitTarget, settingsMap map[edgeproto.RateLimitSettingsKey]*edgeproto.RateLimitSettings) *edgeproto.RateLimitSettings {
	key := edgeproto.RateLimitSettingsKey{
		ApiName:         apiName,
		RateLimitTarget: target,
		ApiEndpointType: edgeproto.ApiEndpointType_DME,
	}
	settings, _ := settingsMap[key]
	return settings
}

// Initialize API RateLimitManager
func initRateLimitMgr() {
	disableRateLimit := uaemcommon.Settings.DisableRateLimit
	if *testMode {
		disableRateLimit = true
	}
	uaemcommon.RateLimitMgr = ratelimit.NewRateLimitManager(disableRateLimit, int(uaemcommon.Settings.RateLimitMaxTrackedIps), 0)
}

func main() {
	nodeMgr.InitFlags()
	nodeMgr.AccessKeyClient.InitFlags()
	flag.Parse()
	log.SetDebugLevelStrs(*debugLevels)
	done := make(chan struct{})
	defer func() {
		close(done)
	}()

	if os.Getenv("E2ETEST_NORANDOM") == "true" {
		uaemcommon.OptionFindCloudletRandomizeVeryClose = false
	}

	var myCertIssuer string
	if *cloudletDme {
		// DME running on Cloudlet infra, requires access key
		myCertIssuer = svcnode.CertIssuerRegionalCloudlet
	} else {
		// Regional DME not associated with any Cloudlet
		myCertIssuer = svcnode.CertIssuerRegional
	}
	cloudcommon.ParseMyCloudletKey(false, cloudletKeyStr, &uaemcommon.MyCloudletKey)
	ctx, span, err := nodeMgr.Init(svcnode.SvcNodeTypeDME, myCertIssuer, svcnode.WithName(*uaemcommon.ScaleID), svcnode.WithCloudletKey(&uaemcommon.MyCloudletKey), svcnode.WithRegion(*region))
	if err != nil {
		log.FatalLog("Failed init node", "err", err)
	}
	defer nodeMgr.Finish()
	operatorApiGw, err = initOperator(ctx, *carrier)
	if err != nil {
		span.Finish()
		log.FatalLog("Failed init plugin", "operator", *carrier, "err", err)
	}
	var servers = operator.OperatorApiGwServers{VaultAddr: nodeMgr.VaultAddr, QosPosUrl: *qosPosUrl, LocVerUrl: *locVerUrl, TokSrvUrl: *tokSrvUrl, QosSesAddr: *qosSesAddr}
	err = operatorApiGw.Init(*carrier, &servers)
	if err != nil {
		span.Finish()
		log.FatalLog("Unable to init API GW", "err", err)

	}
	log.SpanLog(ctx, log.DebugLevelInfo, "plugin init done", "operatorApiGw", operatorApiGw)

	err = uaemcommon.InitVault(nodeMgr.VaultAddr, *region, done)
	if err != nil {
		span.Finish()
		log.FatalLog("Failed to init vault", "err", err)
	}
	if *testMode {
		// init JWK so Vault not required
		uaemcommon.Jwks.Keys[0] = &vault.JWK{
			Secret: "secret",
		}
	}

	eehandler, err := initEdgeEventsPlugin(ctx, *carrier)
	if err != nil {
		span.Finish()
		log.FatalLog("Unable to init EdgeEvents plugin", "err", err)
	}

	uaemcommon.SetupMatchEngine(eehandler)
	grpcOpts := make([]grpc.ServerOption, 0)

	clientTlsConfig, err := nodeMgr.InternalPki.GetClientTlsConfig(ctx,
		nodeMgr.CommonNamePrefix(),
		myCertIssuer,
		[]svcnode.MatchCA{svcnode.SameRegionalMatchCA()})
	if err != nil {
		log.FatalLog("Failed to get notify client tls config", "err", err)
	}
	notifyClientUnaryOp := notify.ClientUnaryInterceptors()
	notifyClientStreamOp := notify.ClientStreamInterceptors()
	if nodeMgr.AccessKeyClient.IsEnabled() {
		notifyClientUnaryOp = notify.ClientUnaryInterceptors(
			nodeMgr.AccessKeyClient.UnaryAddAccessKey)
		notifyClientStreamOp = notify.ClientStreamInterceptors(
			nodeMgr.AccessKeyClient.StreamAddAccessKey)
	}
	notifyClient := initNotifyClient(ctx, *notifyAddrs,
		tls.GetGrpcDialOption(clientTlsConfig),
		notifyClientUnaryOp,
		notifyClientStreamOp,
	)
	sendMetric := notify.NewMetricSend()
	notifyClient.RegisterSend(sendMetric)
	sendAutoProvCounts := notify.NewAutoProvCountsSend()
	notifyClient.RegisterSend(sendAutoProvCounts)
	nodeMgr.RegisterClient(notifyClient)

	// Start autProvStats before we receive Settings Update
	uaemcommon.Settings = *edgeproto.GetDefaultSettings()
	autoProvStats := uaemcommon.InitAutoProvStats(uaemcommon.Settings.AutoDeployIntervalSec, 0, *statsShards, &nodeMgr.MyNode.Key, sendAutoProvCounts.Update)
	autoProvStats.Start()
	defer autoProvStats.Stop()

	notifyClient.Start()
	defer notifyClient.Stop()

	interval := uaemcommon.Settings.DmeApiMetricsCollectionInterval.TimeDuration()
	uaemcommon.Stats = uaemcommon.NewDmeStats(interval, *statsShards, sendMetric.Update)
	uaemcommon.Stats.Start()
	defer uaemcommon.Stats.Stop()

	edgeEventsInterval := uaemcommon.Settings.EdgeEventsMetricsCollectionInterval.TimeDuration()
	uaemcommon.EEStats = uaemcommon.NewEdgeEventStats(edgeEventsInterval, *statsShards, sendMetric.Update)
	uaemcommon.EEStats.Start()
	defer uaemcommon.EEStats.Stop()

	uaemcommon.InitAppInstClients(time.Duration(uaemcommon.Settings.AppinstClientCleanupInterval))
	defer uaemcommon.StopAppInstClients()

	initRateLimitMgr()
	grpcOpts = append(grpcOpts,
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(ratelimit.GetDmeUnaryRateLimiterInterceptor(uaemcommon.RateLimitMgr), uaemcommon.UnaryAuthInterceptor, uaemcommon.Stats.UnaryStatsInterceptor)),
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(ratelimit.GetDmeStreamRateLimiterInterceptor(uaemcommon.RateLimitMgr), uaemcommon.GetStreamAuthInterceptor(), uaemcommon.Stats.GetStreamStatsInterceptor())))

	lis, err := net.Listen("tcp", *apiAddr)
	if err != nil {
		span.Finish()
		log.FatalLog("Failed to listen", "addr", *apiAddr, "err", err)
	}

	// Setup AccessApi for dme
	var accessApi platform.AccessApi
	if *cloudletDme {
		// Setup connection to controller for access API
		if !nodeMgr.AccessKeyClient.IsEnabled() {
			span.Finish()
			log.FatalLog("access key client is not enabled")
		}
		accessApi = accessapicloudlet.NewControllerClient(nodeMgr.AccessApiClient)
	} else {
		// DME has direct access to vault
		cloudlet := &edgeproto.Cloudlet{
			Key: uaemcommon.MyCloudletKey,
		}
		aa := accessapi.NewVaultClient(ctx, nodeMgr.VaultConfig, nil, *region, "", nodeMgr.ValidDomains)
		accessApi = aa.CloudletContext(cloudlet)
	}

	var getPublicCertApi cloudcommon.GetPublicCertApi
	if nodeMgr.InternalPki.UseVaultPki {
		if tls.IsTestTls() || *testMode {
			getPublicCertApi = &cloudcommon.TestPublicCertApi{}
		} else {
			getPublicCertApi = accessApi
		}
	}

	// Convert common name prefix to _.xyz name for cert issuing
	// This is because each dme is given a region.dme.edgecloud.net common name,
	// and we want to issue a single cert for all *.dme.edgecloud.net.
	certCommonNamePrefix := nodeMgr.CommonNamePrefix()
	commonNameParts := strings.Split(certCommonNamePrefix, ".")
	commonNameParts[0] = "_"
	certCommonNamePrefix = strings.Join(commonNameParts, ".")

	// Setup PublicCertManager for dme
	var publicCertManager *svcnode.PublicCertManager
	if publicTls := os.Getenv("PUBLIC_ENDPOINT_TLS"); publicTls == "false" {
		publicCertManager, err = svcnode.NewPublicCertManager(certCommonNamePrefix, nodeMgr.ValidDomains, nil, "", "")
	} else {
		publicCertManager, err = svcnode.NewPublicCertManager(certCommonNamePrefix, nodeMgr.ValidDomains, getPublicCertApi, *tlsApiCertFile, *tlsApiKeyFile)
	}
	if err != nil {
		span.Finish()
		log.FatalLog("unable to get public cert manager", "err", err)
	}
	publicCertManager.StartRefresh()
	// Get TLS Config for grpc Creds from PublicCertManager
	dmeServerTlsConfig, err := publicCertManager.GetServerTlsConfig(ctx)
	if err != nil {
		span.Finish()
		log.FatalLog("get TLS config for grpc server failed", "err", err)
	}
	grpcOpts = append(grpcOpts, cloudcommon.GrpcCreds(dmeServerTlsConfig),
		grpc.ForceServerCodec(&cloudcommon.ProtoCodec{}))

	s := grpc.NewServer(grpcOpts...)

	serv := &server{}
	dme.RegisterMatchEngineApiServer(s, serv)
	dme.RegisterLocationServer(s, serv)
	dme.RegisterQosPositionKpiServer(s, serv)
	dme.RegisterQualityOfServiceServer(s, serv)
	dme.RegisterSessionServer(s, serv)
	dme.RegisterPlatformOSServer(s, serv)

	// Add Global DME RateLimitSettings
	addDmeApiRateLimitSettings(ctx, edgeproto.GlobalApiName)
	// Search for specific APIs to add RateLimitSettings for
	grpcServices := s.GetServiceInfo()
	for _, serviceInfo := range grpcServices {
		for _, methodInfo := range serviceInfo.Methods {
			if strings.Contains(methodInfo.Name, "VerifyLocation") {
				// Add VerifyLocation RateLimitSettings
				addDmeApiRateLimitSettings(ctx, methodInfo.Name)
				break
			}
		}
	}

	InitDebug(&nodeMgr)

	// Register reflection service on gRPC server.
	reflection.Register(s)
	go func() {
		if err := s.Serve(lis); err != nil {
			span.Finish()
			log.FatalLog("Failed to server", "err", err)
		}
	}()
	defer s.Stop()

	// REST service
	mux := http.NewServeMux()
	gwcfg := &cloudcommon.GrpcGWConfig{
		ApiAddr:     *apiAddr,
		TlsCertFile: *tlsApiCertFile,
		ApiHandles: []func(context.Context, *gwruntime.ServeMux, *grpc.ClientConn) error{
			dme.RegisterMatchEngineApiHandler,
			dme.RegisterLocationHandler,
			dme.RegisterQosPositionKpiHandler,
			dme.RegisterQualityOfServiceHandler,
			dme.RegisterSessionHandler,
			dme.RegisterPlatformOSHandler,
		},
	}
	if clientTlsConfig != nil && publicCertManager.TLSMode() != tls.NoTLS {
		gwcfg.GetCertificate = clientTlsConfig.GetClientCertificate
	}

	gw, err := cloudcommon.GrpcGateway(gwcfg)
	if err != nil {
		span.Finish()
		log.FatalLog("Failed to start grpc Gateway", "err", err)
	}
	mux.Handle("/", gw)
	tlscfg, err := publicCertManager.GetServerTlsConfig(ctx)
	if err != nil {
		span.Finish()
		log.FatalLog("get TLS config for http server failed", "err", err)
	}
	if tlscfg != nil {
		tlscfg.CurvePreferences = []ctls.CurveID{ctls.CurveP521, ctls.CurveP384, ctls.CurveP256}
		tlscfg.PreferServerCipherSuites = true
		tlscfg.CipherSuites = []uint16{
			ctls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			ctls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			ctls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			ctls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			ctls.TLS_RSA_WITH_AES_256_CBC_SHA,
		}
	}

	// Suppress contant stream of TLS error logs due to LB health check. There is discussion in the community
	//to get rid of some of these logs, but as of now this a the way around it.   We could miss other logs here but
	// the excessive error logs are drowning out everthing else.
	var nullLogger baselog.Logger
	nullLogger.SetOutput(ioutil.Discard)

	httpServer := &http.Server{
		Addr:      *httpAddr,
		Handler:   allowCORS(mux),
		TLSConfig: tlscfg,
		ErrorLog:  &nullLogger,
	}

	go cloudcommon.GrpcGatewayServe(httpServer, *tlsApiCertFile)
	defer httpServer.Shutdown(context.Background())

	sigChan = make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	log.SpanLog(ctx, log.DebugLevelInfo, "Ready")
	span.Finish()

	// wait until process in killed/interrupted
	sig := <-sigChan
	fmt.Println(sig)
}
