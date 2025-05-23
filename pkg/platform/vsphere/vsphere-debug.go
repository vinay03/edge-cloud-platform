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

package vsphere

import (
	"context"
	"encoding/csv"
	"fmt"
	"strings"

	"github.com/edgexr/edge-cloud-platform/api/edgeproto"
	"github.com/edgexr/edge-cloud-platform/pkg/cloudcommon/svcnode"
)

func (v *VSpherePlatform) initDebug(nodeMgr *svcnode.SvcNodeMgr) {
	nodeMgr.Debug.AddDebugFunc("govccmd", v.runGovcCommand)
}

func (o *VSpherePlatform) runGovcCommand(ctx context.Context, req *edgeproto.DebugRequest) string {
	if req.Args == "" {
		return "please specify govc command in args field"
	}
	rd := csv.NewReader(strings.NewReader(req.Args))
	rd.Comma = ' '
	args, err := rd.Read()
	if err != nil {
		return fmt.Sprintf("failed to split args string, %v", err)
	}
	out, err := o.TimedGovcCommand(ctx, args[0], args[1:]...)
	if err != nil {
		return fmt.Sprintf("govc command failed: %v, %s", err, string(out))
	}
	return string(out)
}
