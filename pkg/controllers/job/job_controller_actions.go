/*
Copyright 2019 The Volcano Authors.

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

package job

import (
	"fmt"
	"sort"
	"sync"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kbv1 "github.com/kubernetes-sigs/kube-batch/pkg/apis/scheduling/v1alpha1"
	kbapi "github.com/kubernetes-sigs/kube-batch/pkg/scheduler/api"
	vkv1 "volcano.sh/volcano/pkg/apis/batch/v1alpha1"
	"volcano.sh/volcano/pkg/apis/helpers"
	"volcano.sh/volcano/pkg/controllers/apis"
	vkjobhelpers "volcano.sh/volcano/pkg/controllers/job/helpers"
	"volcano.sh/volcano/pkg/controllers/job/state"
)

func (cc *Controller) killJob(jobInfo *apis.JobInfo, updateStatus state.UpdateStatusFn) error {
	glog.V(3).Infof("Killing Job <%s/%s>", jobInfo.Job.Namespace, jobInfo.Job.Name)
	defer glog.V(3).Infof("Finished Job <%s/%s> killing", jobInfo.Job.Namespace, jobInfo.Job.Name)

	job := jobInfo.Job
	glog.Infof("Current Version is: %d of job: %s/%s", job.Status.Version, job.Namespace, job.Name)
	if job.DeletionTimestamp != nil {
		glog.Infof("Job <%s/%s> is terminating, skip management process.",
			job.Namespace, job.Name)
		return nil
	}

	var pending, running, terminating, succeeded, failed int32

	var errs []error
	var total int

	for _, pods := range jobInfo.Pods {
		for _, pod := range pods {
			total++

			if pod.DeletionTimestamp != nil {
				glog.Infof("Pod <%s/%s> is terminating", pod.Namespace, pod.Name)
				terminating++
				continue
			}

			if err := cc.deleteJobPod(job.Name, pod); err == nil {
				terminating++
			} else {
				errs = append(errs, err)
				switch pod.Status.Phase {
				case v1.PodRunning:
					running++
				case v1.PodPending:
					pending++
				case v1.PodSucceeded:
					succeeded++
				case v1.PodFailed:
					failed++
				}
			}
		}
	}

	if len(errs) != 0 {
		glog.Errorf("failed to kill pods for job %s/%s, with err %+v", job.Namespace, job.Name, errs)
		return fmt.Errorf("failed to kill %d pods of %d", len(errs), total)
	}

	job = job.DeepCopy()
	//Job version is bumped only when job is killed
	job.Status.Version = job.Status.Version + 1

	job.Status = vkv1.JobStatus{
		State: job.Status.State,

		Pending:      pending,
		Running:      running,
		Succeeded:    succeeded,
		Failed:       failed,
		Terminating:  terminating,
		Version:      job.Status.Version,
		MinAvailable: int32(job.Spec.MinAvailable),
		RetryCount:   job.Status.RetryCount,
	}

	if updateStatus != nil {
		updateStatus(&job.Status)
	}

	// Update Job status
	if job, err := cc.vkClients.BatchV1alpha1().Jobs(job.Namespace).UpdateStatus(job); err != nil {
		glog.Errorf("Failed to update status of Job %v/%v: %v",
			job.Namespace, job.Name, err)
		return err
	} else {
		if e := cc.cache.Update(job); e != nil {
			glog.Errorf("KillJob - Failed to update Job %v/%v in cache:  %v",
				job.Namespace, job.Name, e)
			return e
		}
	}

	// Delete PodGroup
	if err := cc.kbClients.SchedulingV1alpha1().PodGroups(job.Namespace).Delete(job.Name, nil); err != nil {
		if !apierrors.IsNotFound(err) {
			glog.Errorf("Failed to delete PodGroup of Job %v/%v: %v",
				job.Namespace, job.Name, err)
			return err
		}
	}

	if err := cc.pluginOnJobDelete(job); err != nil {
		return err
	}

	// NOTE(k82cn): DO NOT delete input/output until job is deleted.

	return nil
}

func (cc *Controller) createJob(jobInfo *apis.JobInfo, nextState state.UpdateStatusFn) error {
	glog.V(3).Infof("Starting to create Job <%s/%s>", jobInfo.Job.Namespace, jobInfo.Job.Name)
	defer glog.V(3).Infof("Finished Job <%s/%s> create", jobInfo.Job.Namespace, jobInfo.Job.Name)

	job := jobInfo.Job.DeepCopy()
	glog.Infof("Current Version is: %d of job: %s/%s", job.Status.Version, job.Namespace, job.Name)

	if update, err := cc.filljob(job); err != nil || update {
		return err
	}

	if err := cc.pluginOnJobAdd(job); err != nil {
		cc.recorder.Event(job, v1.EventTypeWarning, string(vkv1.PluginError),
			fmt.Sprintf("Execute plugin when job add failed, err: %v", err))
		return err
	}

	if err := cc.createPodGroupIfNotExist(job); err != nil {
		return err
	}

	if err := cc.createJobIOIfNotExist(job); err != nil {
		return err
	}

	if job, err := cc.vkClients.BatchV1alpha1().Jobs(job.Namespace).UpdateStatus(job); err != nil {
		glog.Errorf("Failed to update status of Job %v/%v: %v",
			job.Namespace, job.Name, err)
		return err
	} else {
		if e := cc.cache.Update(job); e != nil {
			glog.Errorf("CreateJob - Failed to update Job %v/%v in cache:  %v",
				job.Namespace, job.Name, e)
			return e
		}
	}

	return nil
}

func (cc *Controller) syncJob(jobInfo *apis.JobInfo, updateStatus state.UpdateStatusFn) error {
	glog.V(3).Infof("Starting to sync up Job <%s/%s>", jobInfo.Job.Namespace, jobInfo.Job.Name)
	defer glog.V(3).Infof("Finished Job <%s/%s> sync up", jobInfo.Job.Namespace, jobInfo.Job.Name)

	job := jobInfo.Job.DeepCopy()
	glog.Infof("Current Version is: %d of job: %s/%s", job.Status.Version, job.Namespace, job.Name)

	if job.DeletionTimestamp != nil {
		glog.Infof("Job <%s/%s> is terminating, skip management process.",
			job.Namespace, job.Name)
		return nil
	}

	var running, pending, terminating, succeeded, failed int32

	var podToCreate []*v1.Pod
	var podToDelete []*v1.Pod
	var creationErrs []error
	var deletionErrs []error

	for _, ts := range job.Spec.Tasks {
		ts.Template.Name = ts.Name
		tc := ts.Template.DeepCopy()
		name := ts.Template.Name

		pods, found := jobInfo.Pods[name]
		if !found {
			pods = map[string]*v1.Pod{}
		}

		for i := 0; i < int(ts.Replicas); i++ {
			podName := fmt.Sprintf(vkjobhelpers.PodNameFmt, job.Name, name, i)
			if pod, found := pods[podName]; !found {
				newPod := createJobPod(job, tc, i)
				if err := cc.pluginOnPodCreate(job, newPod); err != nil {
					return err
				}
				podToCreate = append(podToCreate, newPod)
			} else {
				delete(pods, podName)
				if pod.DeletionTimestamp != nil {
					glog.Infof("Pod <%s/%s> is terminating", pod.Namespace, pod.Name)
					terminating++
					continue
				}

				switch pod.Status.Phase {
				case v1.PodPending:
					pending++
				case v1.PodRunning:
					running++
				case v1.PodSucceeded:
					succeeded++
				case v1.PodFailed:
					failed++
				}
			}
		}

		for _, pod := range pods {
			podToDelete = append(podToDelete, pod)
		}
	}

	waitCreationGroup := sync.WaitGroup{}
	waitCreationGroup.Add(len(podToCreate))
	for _, pod := range podToCreate {
		go func(pod *v1.Pod) {
			defer waitCreationGroup.Done()
			_, err := cc.kubeClients.CoreV1().Pods(pod.Namespace).Create(pod)
			if err != nil && !apierrors.IsAlreadyExists(err) {
				// Failed to create Pod, waitCreationGroup a moment and then create it again
				// This is to ensure all podsMap under the same Job created
				// So gang-scheduling could schedule the Job successfully
				glog.Errorf("Failed to create pod %s for Job %s, err %#v",
					pod.Name, job.Name, err)
				creationErrs = append(creationErrs, err)
			} else {
				pending++
				glog.V(3).Infof("Created Task <%s> of Job <%s/%s>",
					pod.Name, job.Namespace, job.Name)
			}
		}(pod)
	}
	waitCreationGroup.Wait()

	if len(creationErrs) != 0 {
		return fmt.Errorf("failed to create %d pods of %d", len(creationErrs), len(podToCreate))
	}

	// TODO: Can hardly imagine when this is necessary.
	// Delete unnecessary pods.
	waitDeletionGroup := sync.WaitGroup{}
	waitDeletionGroup.Add(len(podToDelete))
	for _, pod := range podToDelete {
		go func(pod *v1.Pod) {
			defer waitDeletionGroup.Done()
			err := cc.deleteJobPod(job.Name, pod)
			if err != nil {
				// Failed to delete Pod, waitCreationGroup a moment and then create it again
				// This is to ensure all podsMap under the same Job created
				// So gang-scheduling could schedule the Job successfully
				glog.Errorf("Failed to delete pod %s for Job %s, err %#v",
					pod.Name, job.Name, err)
				deletionErrs = append(deletionErrs, err)
			} else {
				glog.V(3).Infof("Deleted Task <%s> of Job <%s/%s>",
					pod.Name, job.Namespace, job.Name)
				terminating++
			}
		}(pod)
	}
	waitDeletionGroup.Wait()

	if len(deletionErrs) != 0 {
		return fmt.Errorf("failed to delete %d pods of %d", len(deletionErrs), len(podToDelete))
	}

	job.Status = vkv1.JobStatus{
		State: job.Status.State,

		Pending:             pending,
		Running:             running,
		Succeeded:           succeeded,
		Failed:              failed,
		Terminating:         terminating,
		Version:             job.Status.Version,
		MinAvailable:        int32(job.Spec.MinAvailable),
		ControlledResources: job.Status.ControlledResources,
		RetryCount:          job.Status.RetryCount,
	}

	if updateStatus != nil {
		updateStatus(&job.Status)
	}

	if job, err := cc.vkClients.BatchV1alpha1().Jobs(job.Namespace).UpdateStatus(job); err != nil {
		glog.Errorf("Failed to update status of Job %v/%v: %v",
			job.Namespace, job.Name, err)
		return err
	} else {
		if e := cc.cache.Update(job); e != nil {
			glog.Errorf("SyncJob - Failed to update Job %v/%v in cache:  %v",
				job.Namespace, job.Name, e)
			return e
		}
	}

	return nil
}

func (cc *Controller) calculateVersion(current int32, bumpVersion bool) int32 {
	if current == 0 {
		current += 1
	}
	if bumpVersion {
		current += 1
	}
	return current
}

func (cc *Controller) createJobIOIfNotExist(job *vkv1.Job) error {
	// If PVC does not exist, create them for Job.
	volumes := job.Spec.Volumes
	for _, volume := range volumes {
		vcName := volume.VolumeClaimName
		exist, err := cc.checkPVCExist(job, vcName)
		if err != nil {
			return err
		}
		if !exist {
			if job.Status.ControlledResources == nil {
				job.Status.ControlledResources = make(map[string]string)
			}
			if volume.VolumeClaim != nil {
				if err := cc.createPVC(job, vcName, volume.VolumeClaim); err != nil {
					return err
				}
				job.Status.ControlledResources["volume-pvc-"+vcName] = vcName
			} else {
				job.Status.ControlledResources["volume-emptyDir-"+vcName] = vcName
			}
		}
	}
	return nil
}

func (cc *Controller) needUpdateForVolumeClaim(job *vkv1.Job) (bool, *vkv1.Job, error) {
	// If VolumeClaimName does not exist, generate them for Job.
	var newJob *vkv1.Job
	volumes := job.Spec.Volumes
	update := false
	for index, volume := range volumes {
		vcName := volume.VolumeClaimName
		if len(vcName) == 0 {
			for {
				randomStr := vkjobhelpers.GenRandomStr(12)
				vcName = fmt.Sprintf("%s-volume-%s", job.Name, randomStr)
				exist, err := cc.checkPVCExist(job, vcName)
				if err != nil {
					return false, nil, err
				}
				if exist {
					continue
				}
				if newJob == nil {
					newJob = job.DeepCopy()
				}
				newJob.Spec.Volumes[index].VolumeClaimName = vcName
				update = true
				break
			}
		}
	}
	return update, newJob, nil
}

func (cc *Controller) checkPVCExist(job *vkv1.Job, vcName string) (bool, error) {
	if _, err := cc.pvcLister.PersistentVolumeClaims(job.Namespace).Get(vcName); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		glog.V(3).Infof("Failed to get PVC for job <%s/%s>: %v",
			job.Namespace, job.Name, err)
		return false, err
	}
	return true, nil
}

func (cc *Controller) createPVC(job *vkv1.Job, vcName string, volumeClaim *v1.PersistentVolumeClaimSpec) error {
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: job.Namespace,
			Name:      vcName,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(job, helpers.JobKind),
			},
		},
		Spec: *volumeClaim,
	}

	glog.V(3).Infof("Try to create PVC: %v", pvc)

	if _, e := cc.kubeClients.CoreV1().PersistentVolumeClaims(job.Namespace).Create(pvc); e != nil {
		glog.V(3).Infof("Failed to create PVC for Job <%s/%s>: %v",
			job.Namespace, job.Name, e)
		return e
	}
	return nil
}

func (cc *Controller) createPodGroupIfNotExist(job *vkv1.Job) error {
	// If PodGroup does not exist, create one for Job.
	if _, err := cc.pgLister.PodGroups(job.Namespace).Get(job.Name); err != nil {
		if !apierrors.IsNotFound(err) {
			glog.V(3).Infof("Failed to get PodGroup for Job <%s/%s>: %v",
				job.Namespace, job.Name, err)
			return err
		}
		pg := &kbv1.PodGroup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   job.Namespace,
				Name:        job.Name,
				Annotations: job.Annotations,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(job, helpers.JobKind),
				},
			},
			Spec: kbv1.PodGroupSpec{
				MinMember:    job.Spec.MinAvailable,
				Queue:        job.Spec.Queue,
				MinResources: cc.calcPGMinResources(job),
			},
		}

		if _, e := cc.kbClients.SchedulingV1alpha1().PodGroups(job.Namespace).Create(pg); e != nil {
			glog.V(3).Infof("Failed to create PodGroup for Job <%s/%s>: %v",
				job.Namespace, job.Name, err)

			return e
		}
	}

	return nil
}

func (cc *Controller) deleteJobPod(jobName string, pod *v1.Pod) error {
	err := cc.kubeClients.CoreV1().Pods(pod.Namespace).Delete(pod.Name, nil)
	if err != nil && !apierrors.IsNotFound(err) {
		glog.Errorf("Failed to delete pod %s/%s for Job %s, err %#v",
			pod.Namespace, pod.Name, jobName, err)

		return err
	}

	return nil
}

func (cc *Controller) calcPGMinResources(job *vkv1.Job) *v1.ResourceList {
	// sort task by priorityClasses
	var tasksPriority TasksPriority
	for index := range job.Spec.Tasks {
		tp := TaskPriority{0, job.Spec.Tasks[index]}
		pc := job.Spec.Tasks[index].Template.Spec.PriorityClassName
		if len(cc.priorityClasses) != 0 && cc.priorityClasses[pc] != nil {
			tp.priority = cc.priorityClasses[pc].Value
		}
		tasksPriority = append(tasksPriority, tp)
	}

	sort.Sort(tasksPriority)

	minAvailableTasksRes := kbapi.EmptyResource()
	podCnt := int32(0)
	for _, task := range tasksPriority {
		for i := int32(0); i < task.Replicas; i++ {
			if podCnt >= job.Spec.MinAvailable {
				break
			}
			podCnt++
			for _, c := range task.Template.Spec.Containers {
				minAvailableTasksRes.Add(kbapi.NewResource(c.Resources.Requests))
			}
		}
	}

	return minAvailableTasksRes.Convert2K8sResource()
}

func (cc *Controller) filljob(job *vkv1.Job) (bool, error) {
	update, newJob, err := cc.needUpdateForVolumeClaim(job)
	if err != nil {
		return false, err
	}
	if update {
		if _, err := cc.vkClients.BatchV1alpha1().Jobs(job.Namespace).Update(newJob); err != nil {
			glog.Errorf("Failed to update Job %v/%v: %v",
				job.Namespace, job.Name, err)
			return false, err
		}
		return true, nil
	} else if job.Status.State.Phase == "" {
		job.Status.State.Phase = vkv1.Pending
		if j, err := cc.vkClients.BatchV1alpha1().Jobs(job.Namespace).UpdateStatus(job); err != nil {
			glog.Errorf("Failed to update status of Job %v/%v: %v",
				job.Namespace, job.Name, err)
		} else {
			if e := cc.cache.Update(j); e != nil {
				glog.Error("Failed to update cache status of Job %v/%v: %v", job.Namespace, job.Name, e)
			}
		}
		return true, nil
	}

	return false, nil
}
