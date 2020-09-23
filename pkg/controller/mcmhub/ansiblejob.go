// Copyright 2019 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mcmhub

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	ansiblejob "github.com/open-cluster-management/ansiblejob-go-lib/api/v1alpha1"
	subv1 "github.com/open-cluster-management/multicloud-operators-subscription/pkg/apis/apps/v1"
	"github.com/open-cluster-management/multicloud-operators-subscription/pkg/utils"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Job struct {
	mux sync.Mutex

	Original ansiblejob.AnsibleJob
	Instance []ansiblejob.AnsibleJob
	// track the create instance
	InstanceSet map[types.NamespacedName]struct{}
}

// JobInstances can be applied and can be quired to see if the most applied
// instance is succeeded or not
type JobInstances map[types.NamespacedName]*Job

type appliedJobs struct {
	lastApplied     string
	lastAppliedJobs []string
}

func (jIns *JobInstances) registryJobs(subIns *subv1.Subscription, suffixFunc SuffixFunc,
	jobs []ansiblejob.AnsibleJob, kubeclient client.Client, logger logr.Logger) error {
	for _, job := range jobs {
		jobKey := types.NamespacedName{Name: job.GetName(), Namespace: job.GetNamespace()}
		ins, err := overrideAnsibleInstance(subIns, job, kubeclient, logger)

		if err != nil {
			return err
		}

		if _, ok := (*jIns)[jobKey]; !ok {
			(*jIns)[jobKey] = &Job{
				mux:         sync.Mutex{},
				InstanceSet: make(map[types.NamespacedName]struct{}),
			}
		}

		jobRecords := (*jIns)[jobKey]
		jobRecords.mux.Lock()
		jobRecords.Original = ins

		nx := ins.DeepCopy()
		suffix := suffixFunc(subIns)
		nx.SetName(fmt.Sprintf("%s%s", nx.GetName(), suffix))

		nxKey := types.NamespacedName{Name: nx.GetName(), Namespace: nx.GetNamespace()}

		logger.Info(fmt.Sprintf("registered ansiblejob %s", nxKey))

		if _, ok := jobRecords.InstanceSet[nxKey]; !ok {
			jobRecords.InstanceSet[nxKey] = struct{}{}
			jobRecords.Instance = append(jobRecords.Instance, *nx)
		}

		jobRecords.mux.Unlock()
	}

	return nil
}

// applyjobs will get the original job and create a instance, the applied
// instance will is put into the job.Instance array upon the success of the
// creation
func (jIns *JobInstances) applyJobs(clt client.Client, subIns *subv1.Subscription, logger logr.Logger) error {
	if utils.IsSubscriptionBeDeleted(clt, types.NamespacedName{Name: subIns.GetName(), Namespace: subIns.GetNamespace()}) {
		return nil
	}

	for k, j := range *jIns {
		j.mux.Lock()
		if len(j.Instance) == 0 {
			continue
		}

		n := len(j.Instance)

		nx := j.Instance[n-1]
		//add the created job to the ansiblejob Set
		if err := clt.Create(context.TODO(), &nx); err != nil {
			if !kerr.IsAlreadyExists(err) {
				return fmt.Errorf("failed to apply job %v, err: %v", k.String(), err)
			}
		}

		logger.Info(fmt.Sprintf("applied ansiblejob %s/%s", nx.GetNamespace(), nx.GetName()))

		j.mux.Unlock()
	}

	return nil
}

// check the last instance of the ansiblejobs to see if it's applied and
// completed or not
func (jIns *JobInstances) isJobsCompleted(clt client.Client, logger logr.Logger) (bool, error) {
	for k, job := range *jIns {
		logger.V(DebugLog).Info(fmt.Sprintf("checking if%v job for completed or not", k.String()))

		n := len(job.Instance)
		if n == 0 {
			return true, nil
		}

		j := job.Instance[n-1]
		jKey := types.NamespacedName{Name: j.GetName(), Namespace: j.GetNamespace()}

		if ok, err := isJobDone(clt, jKey, logger); err != nil || !ok {
			return ok, err
		}
	}

	return true, nil
}

func isJobDone(clt client.Client, key types.NamespacedName, logger logr.Logger) (bool, error) {
	job := &ansiblejob.AnsibleJob{}

	if err := clt.Get(context.TODO(), key, job); err != nil {
		// it might not be created by the k8s side yet
		if kerr.IsNotFound(err) {
			return false, nil
		}

		return false, err
	}

	if isJobRunSuccessful(job, logger) {
		return true, nil
	}

	return false, nil
}

type FormatFunc func(ansiblejob.AnsibleJob) string

func ansiblestatusFormat(j ansiblejob.AnsibleJob) string {
	ns := j.GetNamespace()
	if len(ns) == 0 {
		ns = "default"
	}

	return fmt.Sprintf("%s/%s", ns, j.GetName())
}

func formatAnsibleFromTopo(j ansiblejob.AnsibleJob) string {
	ns := j.GetNamespace()
	if len(ns) == 0 {
		ns = "default"
	}

	return fmt.Sprintf("%v/%v/%v/%v/%v/%v", hookParent, "", AnsibleJobKind, ns, j.GetName(), 0)
}

func getJobsString(jobs []ansiblejob.AnsibleJob, format FormatFunc) []string {
	if len(jobs) == 0 {
		return []string{}
	}

	res := []string{}

	for _, j := range jobs {
		res = append(res, format(j))
	}

	return res
}

//merge multiple hook string
func (jIns *JobInstances) outputAppliedJobs(format FormatFunc) appliedJobs {
	res := appliedJobs{}

	lastApplied := []string{}
	lastAppliedJobs := []string{}

	for _, jobRecords := range *jIns {
		if len(jobRecords.Instance) == 0 {
			continue
		}

		jobRecords.mux.Lock()
		applied := getJobsString(jobRecords.Instance, format)

		n := len(applied)
		lastApplied = append(lastApplied, applied[n-1])
		lastAppliedJobs = append(lastAppliedJobs, applied...)
		jobRecords.mux.Unlock()
	}

	sep := ","
	res.lastApplied = strings.Join(lastApplied, sep)
	res.lastAppliedJobs = lastAppliedJobs

	return res
}