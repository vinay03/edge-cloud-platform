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

package dmecommon

import (
	"context"
	"sync"
	"time"

	"github.com/edgexr/edge-cloud-platform/api/edgeproto"
	"github.com/edgexr/edge-cloud-platform/pkg/log"
	"github.com/edgexr/edge-cloud-platform/pkg/util"
	"github.com/gogo/protobuf/types"
)

var autoProvStats *AutoProvStats

type AutoProvStats struct {
	shards      []AutoProvStatsShard
	numShards   uint
	mux         sync.Mutex
	intervalSec float64
	offsetSec   float64
	nodeKey     edgeproto.SvcNodeKey
	send        func(ctx context.Context, counts *edgeproto.AutoProvCounts) bool
	waitGroup   sync.WaitGroup
	stop        chan struct{}
}

type AutoProvStatsShard struct {
	appCloudletCounts map[edgeproto.AppZoneKeyPair]*AutoProvCounts
	mux               sync.Mutex
}

type AutoProvCounts struct {
	count     uint64
	lastCount uint64
}

func InitAutoProvStats(intervalSec, offsetSec float64, numShards uint, nodeKey *edgeproto.SvcNodeKey, send func(ctx context.Context, counts *edgeproto.AutoProvCounts) bool) *AutoProvStats {
	s := AutoProvStats{}
	s.numShards = numShards
	s.intervalSec = intervalSec
	s.offsetSec = offsetSec
	s.send = send
	s.nodeKey = *nodeKey
	s.shards = make([]AutoProvStatsShard, s.numShards, s.numShards)
	for ii, _ := range s.shards {
		s.shards[ii].appCloudletCounts = make(map[edgeproto.AppZoneKeyPair]*AutoProvCounts)
	}
	autoProvStats = &s
	return &s
}

func (s *AutoProvStats) Start() {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.stop != nil {
		// already running
		return
	}
	s.stop = make(chan struct{})
	s.waitGroup.Add(1)
	go s.RunNotify()
}

func (s *AutoProvStats) Stop() {
	s.mux.Lock()
	close(s.stop)
	s.mux.Unlock()
	s.waitGroup.Wait()
	s.mux.Lock()
	s.stop = nil
	s.mux.Unlock()
}

func (s *AutoProvStats) UpdateSettings(intervalSec float64) {
	if s.intervalSec == intervalSec {
		// note that offset should always be 0. Offset allows
		// collector (autoprov service) to collect shortly
		// after generator (dme) pushes stats to influxdb.
		return
	}
	restart := false
	if s.stop != nil {
		s.Stop()
		restart = true
	}
	s.mux.Lock()
	s.intervalSec = intervalSec
	s.mux.Unlock()
	if restart {
		s.Start()
	}
}

func (s *AutoProvStats) Increment(ctx context.Context, appKey *edgeproto.AppKey, zoneKey *edgeproto.ZoneKey, policy *AutoProvPolicy) {
	key := edgeproto.AppZoneKeyPair{
		AppKey:  *appKey,
		ZoneKey: *zoneKey,
	}
	idx := util.GetShardIndex(key, s.numShards)
	shard := &s.shards[idx]

	shard.mux.Lock()
	stats, found := shard.appCloudletCounts[key]
	if !found {
		stats = &AutoProvCounts{}
		shard.appCloudletCounts[key] = stats
	}
	stats.count++
	log.SpanLog(ctx, log.DebugLevelMetrics, "autoprovstats increment", "key", key, "idx", idx, "policy", policy, "stats count", stats.count, "stats last count", stats.lastCount)
	if uint32(stats.count-stats.lastCount) >= policy.DeployClientCount && policy.IntervalCount <= 1 {
		// special case, duration is the same as the interval,
		// and deploy count met, so stats upstream to handle
		// this immediately.
		sendCounts := edgeproto.AutoProvCounts{
			DmeNodeName: s.nodeKey.Name,
			Counts: []*edgeproto.AutoProvCount{
				{
					AppKey:     *appKey,
					ZoneKey:    *zoneKey,
					Count:      stats.count,
					ProcessNow: true,
				},
			},
		}
		s.send(ctx, &sendCounts)
		log.SpanLog(ctx, log.DebugLevelMetrics, "stats immediate auto-prov count", "key", key, "count", stats.count)
	}
	shard.mux.Unlock()
}

func (s *AutoProvStats) RunNotify() {
	done := false
	for !done {
		waitTime := util.GetWaitTime(time.Now(), s.intervalSec, s.offsetSec)
		select {
		case <-time.After(waitTime):
			span := log.StartSpan(log.DebugLevelMetrics, "auto-prov-stats")
			ctx := log.ContextWithSpan(context.Background(), span)

			sendCounts := edgeproto.AutoProvCounts{}
			sendCounts.Counts = make([]*edgeproto.AutoProvCount, 0)
			sendCounts.DmeNodeName = s.nodeKey.Name
			ts, _ := types.TimestampProto(time.Now())
			sendCounts.Timestamp = *ts
			for ii, _ := range s.shards {
				shard := &s.shards[ii]
				shard.mux.Lock()
				for key, stats := range shard.appCloudletCounts {
					if stats.count == stats.lastCount {
						continue
					}
					apCount := edgeproto.AutoProvCount{
						AppKey:  key.AppKey,
						ZoneKey: key.ZoneKey,
						Count:   stats.count,
					}
					stats.lastCount = stats.count
					sendCounts.Counts = append(sendCounts.Counts, &apCount)
					log.SpanLog(ctx, log.DebugLevelMetrics, "stats auto-prov count", "key", key, "count", stats.count)
				}
				shard.mux.Unlock()
			}
			if len(sendCounts.Counts) > 0 {
				s.send(ctx, &sendCounts)
			}
			span.Finish()
		case <-s.stop:
			done = true
		}
	}
	s.waitGroup.Done()
}

func (s *AutoProvStats) Clear(appKey *edgeproto.AppKey, policy string) {
	for ii, _ := range s.shards {
		shard := &s.shards[ii]
		shard.mux.Lock()
		for key, _ := range shard.appCloudletCounts {
			if key.AppKey.Matches(appKey) {
				delete(shard.appCloudletCounts, key)
			}
		}
		shard.mux.Unlock()
	}
}

func (s *AutoProvStats) Prune(apps map[edgeproto.AppKey]struct{}) {
	for ii, _ := range s.shards {
		shard := &s.shards[ii]
		shard.mux.Lock()
		for key, _ := range shard.appCloudletCounts {
			if _, found := apps[key.AppKey]; !found {
				delete(shard.appCloudletCounts, key)
			}
		}
		shard.mux.Unlock()
	}
}
