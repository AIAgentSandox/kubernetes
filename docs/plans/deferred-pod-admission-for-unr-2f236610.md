# Deferred Pod Admission for Unregistered Device Plugins

## Implementation Steps

### Task 1: Deferred Pod Admission for Unregistered Device Plugins

- [x] # Plan: Deferred Pod Admission for Unregistered Device Plugins                                                                                                                                  
                                                              
  Goal: When a pod admission fails because a device plugin hasn't registered yet, defer the pod (instead of rejecting it permanently) and retry until either the device plugin registers or a   
  timeout expires. No changes to the device plugin interface.                                                                                                                                   
   
  Key insight: The error "cannot allocate unregistered device" from devicesToAllocate() (cm/devicemanager/manager.go:654) propagates through topology manager → canAdmitPod → AddPod →          
  HandlePodAdditions, where rejectPod sets the pod to terminal PodFailed. We intercept this at the allocation_manager level.
                                                                                                                                                                                                
  Task 1. Add Defer field to PodAdmitResult                                                                                                                                                     
   
  - [x] In pkg/kubelet/lifecycle/interfaces.go, add a Defer bool field to the PodAdmitResult struct (line 47). When Defer is true, it means admission should be retried later rather than           
  permanently rejected.                                     
                                                                                                                                                                                                
  Task 2. Detect "unregistered device" errors as deferrable in cm/admission/errors.go

  - [x] In pkg/kubelet/cm/admission/errors.go, add a new error type DeviceNotReadyError that implements the existing Error interface, with a distinct Type() string (e.g. "DeviceNotReady").        
  - [x] In pkg/kubelet/cm/devicemanager/manager.go at line 654, wrap the "cannot allocate unregistered device" error with the new DeviceNotReadyError type instead of returning a bare fmt.Errorf.
  This is the only change in the device manager — no new methods.                                                                                                                               
  - [x] In pkg/kubelet/cm/admission/errors.go, update GetPodAdmitResult to detect DeviceNotReadyError (via errors.As) and set Defer: true on the returned PodAdmitResult.
                                                                                                                                                                                                
  Task 3. Propagate Defer through topology manager to canAdmitPod                                                                                                                               
                                                                                                                                                                                                
  - [x] In pkg/kubelet/allocation/allocation_manager.go canAdmitPod (line 588), when a handler returns Admit: false and Defer: true, propagate that deferral state back to the caller rather than   
  immediately rejecting. Add a defer return value or fold it into the existing return tuple (e.g. return (ok, defer, reason, message)).
                                                                                                                                                                                                
  Task 4. Add deferred admission tracking to allocation_manager

  - [x] In pkg/kubelet/allocation/allocation_manager.go, add a podsWithDeferredAdmission map of types.UID → time.Time (the time.Time records when the pod first entered the deferred state) to the  
  manager struct.
  - [x] Add a configurable deferredAdmissionTimeout constant (e.g. 5 * time.Minute). If time.Since(firstDeferredTime) > deferredAdmissionTimeout, stop deferring and reject the pod as before.      
                                                                                                                                                                                                
  Task 5. Modify AddPod to support deferral instead of rejection
                                                                                                                                                                                                
  - [x] In pkg/kubelet/allocation/allocation_manager.go AddPod (line 502), when canAdmitPod indicates deferral:                                                                                     
    - [x] Record the pod UID and current time in podsWithDeferredAdmission (only if not already tracked — preserve the original first-seen time).
    - [x] Return a new deferral indicator to the caller (not a rejection).                                                                                                                          
  - [x] Add a RemoveDeferredPod or reuse RemovePod to clean up deferred state when a pod is deleted.                                                                                                
   
  Task 6. Update Manager interface to expose deferral                                                                                                                                           
                                                            
  - [x] In pkg/kubelet/allocation/allocation_manager.go, update the AddPod method signature (or add a new return value) so the caller can distinguish between rejected and deferred. A minimal      
  approach: change AddPod to return (ok bool, deferred bool, reason, message string).
  - [x] Update the Manager interface (line 88) to match the new signature.                                                                                                                          
  - [x] Add a RetryDeferredAdmissions(ctx context.Context) method to the Manager interface, mirroring RetryPendingResizes.
                                                                                                                                                                                                
  Task 7. Handle deferred pods in HandlePodAdditions
                                                                                                                                                                                                
  - [x] In pkg/kubelet/kubelet.go HandlePodAdditions (line 2854), when AddPod returns deferred=true:
    - [x] Do not call rejectPod (which sets PodFailed — terminal).
    - [x] Do not call podWorkers.UpdatePod (don't start the pod yet).                                                                                                                               
    - [x] Log that admission is deferred.
  - [x] This is the critical change: the pod stays in Pending and is not dispatched to pod workers until admission succeeds or times out.                                                           
                                                            
  Task 8. Implement the retry loop for deferred admissions                                                                                                                                      
                                                            
  - [x] In pkg/kubelet/allocation/allocation_manager.go, implement retryDeferredAdmissions (similar to retryPendingResizes at line 217):                                                            
    - [x] Iterate podsWithDeferredAdmission.
    - [x] For each pod, re-run canAdmitPod.                                                                                                                                                         
    - [x] If admitted: remove from deferred map, call SetAllocatedResources, trigger pod sync via triggerPodSync.                                                                                   
    - [x] If still deferred and within timeout: keep in map.
    - [x] If timed out: remove from map, reject the pod (call a rejection callback).                                                                                                                
  - [x] In Run() (line 194), add deferred admission retries to the existing ticker loop alongside pending resizes.                                                                                  
   
  Task 9. Wire up rejection callback for timed-out deferred pods                                                                                                                                
                                                            
  - [x] Define a predefined constant deferredAdmissionTimeout = 1 * time.Minute in pkg/kubelet/allocation/allocation_manager.go. This is the maximum duration a pod can remain in the deferred      
  admission queue before being permanently rejected. If a pod has been continuously deferred for longer than this timeout (measured from when it first entered the deferred state), it is
  rejected with PodFailed status, matching the existing immediate-rejection behavior.                                                                                                           
  - [x] The allocation manager needs a way to reject pods that have timed out. Add a rejectPod func(ctx, pod, reason, message) callback to the manager struct, passed in from NewManager.
  - [x] In kubelet.go where NewManager is called, pass kl.rejectPod as the rejection callback.                                                                                                      
  - [x] When a deferred pod times out in retryDeferredAdmissions, call this callback to set PodFailed status, matching the existing behavior.                                                       
                                                                                                                                                                                                
  Task 10. Clean up deferred state on pod removal                                                                                                                                               
                                                            
  - [x] In RemovePod (line 528), also remove the pod from podsWithDeferredAdmission if present.                                                                                                     
  - [x] In RemoveOrphanedPods (line 535), also clean up deferred pods that are no longer in the remaining set.
                                                                                                                                                                                                
  Task 11. Unit tests
                                                                                                                                                                                                
  - [x] Add unit tests for allocation_manager covering:         
    - [x] Pod deferred when handler returns Defer: true — not rejected, tracked with timestamp.
    - [x] Retry succeeds when device becomes available — pod admitted and synced.
    - [x] Retry fails after timeout — pod rejected with PodFailed.                                                                                                                                  
    - [x] Pod removed while deferred — cleaned up properly.
    - [x] Non-device-plugin rejections still reject immediately (no deferral).                                                                                                                      
  - [x] Add unit test for DeviceNotReadyError in cm/admission/errors.go — verify GetPodAdmitResult sets Defer: true.                                                                                
  - [x] Add unit test for devicesToAllocate returning DeviceNotReadyError when device is unregistered.
                                                                                                                                                                                                
  Task 12. e2e node test                                    
                                                                                                                                                                                                
  - [x] Add an e2e node test in test/e2e_node/device_plugin_failures_test.go (or a new file alongside it) following the patterns in that file:                                                      
    - [x] Deploy a pod requesting a device plugin resource before the device plugin has registered.
    - [x] Verify that the pod remains in Pending state (not Failed) while the device plugin is unregistered.                                                                                        
    - [x] Start the device plugin (register it with the kubelet).                                                                                                                                   
    - [x] Verify that the pod is admitted and transitions to Running after the device plugin registers.                                                                                             
    - [x] Test the timeout path: deploy a pod requesting a non-existent device plugin resource and verify that after the 1-minute timeout, the pod is rejected with PodFailed.                      
    - [x] Use the existing sample device plugin test helpers (getSampleDevicePluginPod, stub device plugin deployment) from device_plugin_failures_test.go as the basis.                            
                                                                                                                                                                                                
  ---                                                                                                                                                                                           
  Files changed (minimal footprint for cherry-pick):                                                                                                                                            
  1. pkg/kubelet/lifecycle/interfaces.go — add Defer field                                                                                                                                      
  2. pkg/kubelet/cm/admission/errors.go — add DeviceNotReadyError, update GetPodAdmitResult
  3. pkg/kubelet/cm/devicemanager/manager.go — wrap error at line 654                                                                                                                           
  4. pkg/kubelet/allocation/allocation_manager.go — deferred queue, retry loop, interface changes                                                                                               
  5. pkg/kubelet/kubelet.go — handle deferred result in HandlePodAdditions, wire rejection callback
  6. test/e2e_node/device_plugin_failures_test.go — e2e node test for deferred admission  
