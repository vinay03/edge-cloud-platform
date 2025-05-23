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

package controller

import (
	"context"
	"fmt"

	"github.com/edgexr/edge-cloud-platform/api/edgeproto"
	"github.com/edgexr/edge-cloud-platform/pkg/log"
	"github.com/edgexr/edge-cloud-platform/pkg/regiondata"
	"go.etcd.io/etcd/client/v3/concurrency"
)

type VMPoolApi struct {
	all   *AllApis
	sync  *regiondata.Sync
	store edgeproto.VMPoolStore
	cache edgeproto.VMPoolCache
}

func NewVMPoolApi(sync *regiondata.Sync, all *AllApis) *VMPoolApi {
	vmPoolApi := VMPoolApi{}
	vmPoolApi.all = all
	vmPoolApi.sync = sync
	vmPoolApi.store = edgeproto.NewVMPoolStore(sync.GetKVStore())
	edgeproto.InitVMPoolCache(&vmPoolApi.cache)
	sync.RegisterCache(&vmPoolApi.cache)
	return &vmPoolApi
}

func (s *VMPoolApi) CreateVMPool(ctx context.Context, in *edgeproto.VMPool) (*edgeproto.Result, error) {
	if err := in.Validate(nil); err != nil {
		return &edgeproto.Result{}, err
	}
	err := s.sync.ApplySTMWait(ctx, func(stm concurrency.STM) error {
		if s.store.STMGet(stm, &in.Key, nil) {
			return in.Key.ExistsError()
		}
		s.store.STMPut(stm, in)
		return nil
	})
	return &edgeproto.Result{}, err
}

func (s *VMPoolApi) UpdateVMPool(ctx context.Context, in *edgeproto.VMPool) (*edgeproto.Result, error) {
	err := in.ValidateUpdateFields()
	if err != nil {
		return &edgeproto.Result{}, err
	}
	if err := in.Validate(nil); err != nil {
		return &edgeproto.Result{}, err
	}

	cctx := DefCallContext()
	cctx.SetOverride(&in.CrmOverride)

	// Let platform update the pool, if the pool is in use by Cloudlet
	cloudlet := s.all.cloudletApi.GetCloudletForVMPool(&in.Key)
	if cloudlet != nil && !ignoreCRM(cctx) {
		updateVMs := make(map[string]edgeproto.VM)
		for _, vm := range in.Vms {
			if vm.State != edgeproto.VMState_VM_FORCE_FREE {
				vm.State = edgeproto.VMState_VM_UPDATE
			}
			updateVMs[vm.Name] = vm
		}
		err = s.updateVMPoolInternal(cctx, ctx, cloudlet, &in.Key, updateVMs)
	} else {
		err = s.sync.ApplySTMWait(ctx, func(stm concurrency.STM) error {
			cur := edgeproto.VMPool{}
			changed := 0
			if !s.store.STMGet(stm, &in.Key, &cur) {
				return in.Key.NotFoundError()
			}
			changed = cur.CopyInFields(in)
			if err := cur.Validate(nil); err != nil {
				return err
			}
			for ii, vm := range cur.Vms {
				if vm.State == edgeproto.VMState_VM_FORCE_FREE {
					cur.Vms[ii].State = edgeproto.VMState_VM_FREE
					changed += 1
				}
			}
			if changed == 0 {
				return nil
			}
			cur.State = edgeproto.TrackedState_READY
			s.store.STMPut(stm, &cur)
			return nil
		})
	}
	return &edgeproto.Result{}, err
}

func (s *VMPoolApi) DeleteVMPool(ctx context.Context, in *edgeproto.VMPool) (res *edgeproto.Result, reterr error) {
	cctx := DefCallContext()
	cctx.SetOverride(&in.CrmOverride)
	err := s.sync.ApplySTMWait(ctx, func(stm concurrency.STM) error {
		cur := edgeproto.VMPool{}
		if !s.store.STMGet(stm, &in.Key, &cur) {
			return in.Key.NotFoundError()
		}
		if !ignoreCRMTransient(cctx) && cur.DeletePrepare {
			return in.Key.BeingDeletedError()
		}
		cur.DeletePrepare = true
		s.store.STMPut(stm, &cur)
		return nil
	})
	if err != nil {
		return &edgeproto.Result{}, err
	}
	defer func() {
		if reterr == nil {
			return
		}
		undoErr := s.sync.ApplySTMWait(ctx, func(stm concurrency.STM) error {
			cur := edgeproto.VMPool{}
			if !s.store.STMGet(stm, &in.Key, &cur) {
				return nil
			}
			if cur.DeletePrepare {
				cur.DeletePrepare = false
				s.store.STMPut(stm, &cur)
			}
			return nil
		})
		if undoErr != nil {
			log.SpanLog(ctx, log.DebugLevelApi, "Failed to undo delete prepare", "key", in.Key, "err", undoErr)
		}
	}()

	// Validate if pool is in use by Cloudlet
	if s.all.cloudletApi.GetCloudletForVMPool(&in.Key) != nil {
		return &edgeproto.Result{}, fmt.Errorf("VM pool in use by Cloudlet")
	}
	err = s.sync.ApplySTMWait(ctx, func(stm concurrency.STM) error {
		if !s.store.STMGet(stm, &in.Key, nil) {
			return in.Key.NotFoundError()
		}
		s.store.STMDel(stm, &in.Key)
		return nil
	})
	return &edgeproto.Result{}, err
}

func (s *VMPoolApi) ShowVMPool(in *edgeproto.VMPool, cb edgeproto.VMPoolApi_ShowVMPoolServer) error {
	err := s.cache.Show(in, func(obj *edgeproto.VMPool) error {
		err := cb.Send(obj)
		return err
	})
	return err
}

func (s *VMPoolApi) AddVMPoolMember(ctx context.Context, in *edgeproto.VMPoolMember) (*edgeproto.Result, error) {
	if err := in.Validate(nil); err != nil {
		return &edgeproto.Result{}, err
	}

	cctx := DefCallContext()
	cctx.SetOverride(&in.CrmOverride)

	var err error
	// Let platform update the pool, if the pool is in use by Cloudlet
	cloudlet := s.all.cloudletApi.GetCloudletForVMPool(&in.Key)
	if cloudlet != nil && !ignoreCRM(cctx) {
		updateVMs := make(map[string]edgeproto.VM)
		in.Vm.State = edgeproto.VMState_VM_ADD
		updateVMs[in.Vm.Name] = in.Vm
		err = s.updateVMPoolInternal(cctx, ctx, cloudlet, &in.Key, updateVMs)
	} else {
		err = s.sync.ApplySTMWait(ctx, func(stm concurrency.STM) error {
			cur := edgeproto.VMPool{}
			if !s.store.STMGet(stm, &in.Key, &cur) {
				return in.Key.NotFoundError()
			}
			poolMember := edgeproto.VMPoolMember{}
			poolMember.Key = in.Key
			for ii, _ := range cur.Vms {
				if cur.Vms[ii].Name == in.Vm.Name {
					return fmt.Errorf("VM with same name already exists as part of VM pool")
				}
				err := validateVMNetInfo(&cur.Vms[ii], &in.Vm)
				if err != nil {
					return err
				}
			}
			cur.Vms = append(cur.Vms, in.Vm)
			cur.State = edgeproto.TrackedState_READY
			s.store.STMPut(stm, &cur)
			return nil
		})
	}
	return &edgeproto.Result{}, err
}

func (s *VMPoolApi) RemoveVMPoolMember(ctx context.Context, in *edgeproto.VMPoolMember) (*edgeproto.Result, error) {
	if err := in.Key.ValidateKey(); err != nil {
		return &edgeproto.Result{}, err
	}
	if err := in.Vm.ValidateName(); err != nil {
		return &edgeproto.Result{}, err
	}

	var err error

	cctx := DefCallContext()
	cctx.SetOverride(&in.CrmOverride)

	// Let platform update the pool, if the pool is in use by Cloudlet
	cloudlet := s.all.cloudletApi.GetCloudletForVMPool(&in.Key)
	if cloudlet != nil && !ignoreCRM(cctx) {
		updateVMs := make(map[string]edgeproto.VM)
		in.Vm.State = edgeproto.VMState_VM_REMOVE
		updateVMs[in.Vm.Name] = in.Vm
		err = s.updateVMPoolInternal(cctx, ctx, cloudlet, &in.Key, updateVMs)
	} else {
		err = s.sync.ApplySTMWait(ctx, func(stm concurrency.STM) error {
			cur := edgeproto.VMPool{}
			if !s.store.STMGet(stm, &in.Key, &cur) {
				return in.Key.NotFoundError()
			}
			changed := false
			for ii, vm := range cur.Vms {
				if vm.Name == in.Vm.Name {
					cur.Vms = append(cur.Vms[:ii], cur.Vms[ii+1:]...)
					changed = true
					break
				}
			}
			if !changed {
				return nil
			}
			cur.State = edgeproto.TrackedState_READY
			s.store.STMPut(stm, &cur)
			return nil
		})

	}

	return &edgeproto.Result{}, err
}

// Transition states indicate states in which the CRM is still busy.
var UpdateVMPoolTransitions = map[edgeproto.TrackedState]struct{}{
	edgeproto.TrackedState_UPDATING: struct{}{},
}

func validateVMNetInfo(vmCur, vmNew *edgeproto.VM) error {
	if vmCur.NetInfo.ExternalIp != "" {
		if vmCur.NetInfo.ExternalIp == vmNew.NetInfo.ExternalIp {
			return fmt.Errorf("VM with same external IP already exists as part of VM pool")
		}
	}
	if vmCur.NetInfo.InternalIp != "" {
		if vmCur.NetInfo.InternalIp == vmNew.NetInfo.InternalIp {
			return fmt.Errorf("VM with same internal IP already exists as part of VM pool")
		}
	}
	return nil
}

func (s *VMPoolApi) startVMPoolStream(ctx context.Context, cctx *CallContext, streamCb *CbWrapper, modRev int64) (*streamSend, error) {
	streamSendObj, err := s.all.streamObjApi.startStream(ctx, cctx, streamCb, modRev)
	if err != nil {
		log.SpanLog(ctx, log.DebugLevelApi, "failed to start VMPool stream", "err", err)
		return nil, err
	}
	return streamSendObj, nil
}

func (s *VMPoolApi) stopVMPoolStream(ctx context.Context, cctx *CallContext, key *edgeproto.VMPoolKey, streamSendObj *streamSend, objErr error, cleanupStream CleanupStreamAction) {
	if err := s.all.streamObjApi.stopStream(ctx, cctx, key.StreamKey(), streamSendObj, objErr, cleanupStream); err != nil {
		log.SpanLog(ctx, log.DebugLevelApi, "failed to stop VMPool stream", "err", err)
	}
}

func (s *VMPoolApi) updateVMPoolInternal(cctx *CallContext, ctx context.Context, cloudlet *edgeproto.Cloudlet, key *edgeproto.VMPoolKey, vms map[string]edgeproto.VM) (reterr error) {
	if len(vms) == 0 {
		return fmt.Errorf("no VMs specified")
	}
	log.SpanLog(ctx, log.DebugLevelApi, "UpdateVMPoolInternal", "key", key, "vms", vms)

	modRev, err := s.sync.ApplySTMWaitRev(ctx, func(stm concurrency.STM) error {
		cur := edgeproto.VMPool{}
		if !s.store.STMGet(stm, key, &cur) {
			return key.NotFoundError()
		}
		if cur.State == edgeproto.TrackedState_UPDATE_REQUESTED && !ignoreTransient(cctx, cur.State) {
			return fmt.Errorf("Action already in progress, please try again later")
		}
		for ii, vm := range cur.Vms {
			for updateVMName, updateVM := range vms {
				if vm.Name == updateVMName {
					if updateVM.State == edgeproto.VMState_VM_ADD {
						return fmt.Errorf("VM %s already exists as part of VM pool", vm.Name)
					}
				} else {
					if updateVM.State == edgeproto.VMState_VM_ADD || updateVM.State == edgeproto.VMState_VM_UPDATE {
						err := validateVMNetInfo(&vm, &updateVM)
						if err != nil {
							return err
						}
					}
				}
			}
			updateVM, ok := vms[vm.Name]
			if !ok {
				continue
			}
			cur.Vms[ii] = updateVM
			delete(vms, vm.Name)
		}
		for vmName, vm := range vms {
			if vm.State == edgeproto.VMState_VM_REMOVE {
				return fmt.Errorf("VM %s does not exist in the pool", vmName)
			}
			cur.Vms = append(cur.Vms, vm)
		}
		log.SpanLog(ctx, log.DebugLevelApi, "Update VMPool", "newPool", cur)
		if !ignoreCRM(cctx) {
			cur.State = edgeproto.TrackedState_UPDATE_REQUESTED
		}
		s.store.STMPut(stm, &cur)
		return nil
	})
	if err != nil {
		return err
	}
	if ignoreCRM(cctx) {
		return nil
	}
	streamCb, _ := s.all.streamObjApi.newStream(ctx, cctx, key.StreamKey(), nil)
	sendObj, err := s.startVMPoolStream(ctx, cctx, streamCb, modRev)
	if err != nil {
		return err
	}
	defer func() {
		s.stopVMPoolStream(ctx, cctx, key, sendObj, reterr, CleanupStream)
	}()
	reqCtx, reqCancel := context.WithTimeout(ctx, s.all.settingsApi.Get().UpdateVmPoolTimeout.TimeDuration())
	defer reqCancel()

	if cloudlet.CrmOnEdge {
		err = edgeproto.WaitForVMPoolInfo(reqCtx, key, s.store, edgeproto.TrackedState_READY,
			UpdateVMPoolTransitions, edgeproto.TrackedState_UPDATE_ERROR,
			"Updated VM Pool Successfully", nil,
			sendObj.crmMsgCh)
	} else {
		// VMPool not supported for CCRM
		return fmt.Errorf("vm pool not supported off edge-site")
	}
	// State state back to Unknown & Error to nil, as user is notified about the error, if any
	s.sync.ApplySTMWait(ctx, func(stm concurrency.STM) error {
		cur := edgeproto.VMPool{}
		if !s.store.STMGet(stm, key, &cur) {
			return key.NotFoundError()
		}
		cur.State = edgeproto.TrackedState_TRACKED_STATE_UNKNOWN
		cur.Errors = nil
		s.store.STMPut(stm, &cur)
		return nil
	})
	return err
}

func (s *VMPoolApi) UpdateFromInfo(ctx context.Context, in *edgeproto.VMPoolInfo) {
	log.SpanLog(ctx, log.DebugLevelApi, "Update VM pool from info", "info", in)

	fmap := edgeproto.MakeFieldMap(in.Fields)

	// publish the received info object on redis
	s.all.streamObjApi.UpdateStatus(ctx, in, &in.State, nil, in.Key.StreamKey())

	s.sync.ApplySTMWait(ctx, func(stm concurrency.STM) error {
		vmPool := edgeproto.VMPool{}
		if !s.store.STMGet(stm, &in.Key, &vmPool) {
			// got deleted in the meantime
			return nil
		}
		if fmap.HasOrHasChild(edgeproto.VMPoolInfoFieldVms) {
			vmPool.Vms = in.Vms
		}
		if fmap.Has(edgeproto.VMPoolInfoFieldState) {
			vmPool.State = in.State
		}
		if fmap.Has(edgeproto.VMPoolInfoFieldErrors) {
			vmPool.Errors = in.Errors
		}
		s.store.STMPut(stm, &vmPool)
		return nil
	})
}
