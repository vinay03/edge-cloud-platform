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

package autoprov

import (
	"github.com/edgexr/edge-cloud-platform/api/edgeproto"
	"github.com/edgexr/edge-cloud-platform/pkg/cloudcommon/svcnode"
	"github.com/edgexr/edge-cloud-platform/pkg/notify"
)

type CacheData struct {
	appCache            edgeproto.AppCache
	appInstCache        edgeproto.AppInstCache
	appInstRefsCache    edgeproto.AppInstRefsCache
	autoProvPolicyCache edgeproto.AutoProvPolicyCache
	zoneCache           edgeproto.ZoneCache
	cloudletCache       *edgeproto.CloudletCache
	cloudletInfoCache   edgeproto.CloudletInfoCache
	frClusterInsts      edgeproto.FreeReservableClusterInstCache
	alertCache          edgeproto.AlertCache
	autoProvInfoCache   edgeproto.AutoProvInfoCache
}

func (s *CacheData) init(nodeMgr *svcnode.SvcNodeMgr) {
	edgeproto.InitAppCache(&s.appCache)
	edgeproto.InitAppInstCache(&s.appInstCache)
	edgeproto.InitAppInstRefsCache(&s.appInstRefsCache)
	edgeproto.InitAutoProvPolicyCache(&s.autoProvPolicyCache)
	edgeproto.InitZoneCache(&s.zoneCache)
	if nodeMgr != nil {
		s.cloudletCache = nodeMgr.CloudletLookup.GetCloudletCache(svcnode.NoRegion)
	} else {
		s.cloudletCache = &edgeproto.CloudletCache{}
		edgeproto.InitCloudletCache(s.cloudletCache)
	}
	edgeproto.InitCloudletInfoCache(&s.cloudletInfoCache)
	s.frClusterInsts.Init()
	edgeproto.InitAlertCache(&s.alertCache)
	edgeproto.InitAutoProvInfoCache(&s.autoProvInfoCache)
}

func (s *CacheData) initNotifyClient(client *notify.Client) {
	notifyClient.RegisterRecvAppCache(&s.appCache)
	notifyClient.RegisterRecvAppInstCache(&s.appInstCache)
	notifyClient.RegisterRecvAppInstRefsCache(&s.appInstRefsCache)
	notifyClient.RegisterRecvAutoProvPolicyCache(&s.autoProvPolicyCache)
	notifyClient.RegisterRecvZoneCache(&s.zoneCache)
	notifyClient.RegisterRecvCloudletCache(s.cloudletCache)
	notifyClient.RegisterRecvCloudletInfoCache(&s.cloudletInfoCache)
	notifyClient.RegisterRecv(notify.NewClusterInstRecv(&s.frClusterInsts))
	notifyClient.RegisterRecvAlertCache(&s.alertCache)
	notifyClient.RegisterSendAutoProvInfoCache(&s.autoProvInfoCache)
}
