/*
Copyright 2016 The Kubernetes Authors.

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

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	corev1api "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntimelog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	prowapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	kubernetesreporterapi "k8s.io/test-infra/prow/crier/reporters/gcs/kubernetes/api"
	"k8s.io/test-infra/prow/flagutil"
	"k8s.io/test-infra/prow/interrupts"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/logrusutil"
	"k8s.io/test-infra/prow/metrics"
	"k8s.io/test-infra/prow/pjutil"
	"k8s.io/test-infra/prow/version"
)

type options struct {
	runOnce                    bool
	cleanDuplicates            bool
	configPath                 string
	jobConfigPath              string
	supplementalProwConfigDirs flagutil.Strings
	dryRun                     flagutil.Bool
	kubernetes                 flagutil.KubernetesOptions
	instrumentationOptions     flagutil.InstrumentationOptions
}

const (
	reasonPodAged     = "aged"
	reasonPodOrphaned = "orphaned"
	reasonPodTTLed    = "ttled"

	reasonProwJobAged         = "aged"
	reasonDuplicate           = "duplicate"
	reasonProwJobAgedPeriodic = "aged-periodic"
)

func gatherOptions(fs *flag.FlagSet, args ...string) options {
	o := options{}
	fs.BoolVar(&o.runOnce, "run-once", false, "If true, run only once then quit.")
	fs.BoolVar(&o.cleanDuplicates, "clean-duplicate-prowjobs", false, "If sinker should instantly clean up duplicate prowjobs. Jobs that are not a periodic, did complete and have the same name, refs, extra refs and state are considered duplicates.")
	fs.StringVar(&o.configPath, "config-path", "", "Path to config.yaml.")
	fs.StringVar(&o.jobConfigPath, "job-config-path", "", "Path to prow job configs.")
	fs.Var(&o.supplementalProwConfigDirs, "supplemental-prow-config-dir", "An additional directory from which to load prow configs. Can be used for config sharding but only supports a subset of the config. The flag can be passed multiple times.")

	// TODO(fejta): switch dryRun to be a bool, defaulting to true after March 15, 2019.
	fs.Var(&o.dryRun, "dry-run", "Whether or not to make mutating API calls to Kubernetes.")

	o.kubernetes.AddFlags(fs)
	o.instrumentationOptions.AddFlags(fs)
	fs.Parse(args)
	return o
}

func (o *options) Validate() error {
	if err := o.kubernetes.Validate(o.dryRun.Value); err != nil {
		return err
	}

	if o.configPath == "" {
		return errors.New("--config-path is required")
	}

	return nil
}

func main() {
	logrusutil.ComponentInit()

	o := gatherOptions(flag.NewFlagSet(os.Args[0], flag.ExitOnError), os.Args[1:]...)
	if err := o.Validate(); err != nil {
		logrus.WithError(err).Fatal("Invalid options")
	}

	defer interrupts.WaitForGracefulShutdown()

	pjutil.ServePProf(o.instrumentationOptions.PProfPort)

	if !o.dryRun.Explicit {
		logrus.Warning("Sinker requires --dry-run=false to function correctly in production.")
		logrus.Warning("--dry-run will soon default to true. Set --dry-run=false by March 15.")
	}

	configAgent := &config.Agent{}
	if err := configAgent.Start(o.configPath, o.jobConfigPath, o.supplementalProwConfigDirs.Strings()); err != nil {
		logrus.WithError(err).Fatal("Error starting config agent.")
	}
	cfg := configAgent.Config

	metrics.ExposeMetrics("sinker", cfg().PushGateway, o.instrumentationOptions.MetricsPort)

	ctrlruntimelog.SetLogger(zap.New(zap.JSONEncoder()))

	infrastructureClusterConfig, err := o.kubernetes.InfrastructureClusterConfig(o.dryRun.Value)
	if err != nil {
		logrus.WithError(err).Fatal("Error getting config for infastructure cluster")
	}

	// The watch apimachinery doesn't support restarts, so just exit the binary if a kubeconfig changes
	// to make the kubelet restart us.
	if err := o.kubernetes.AddKubeconfigChangeCallback(func() {
		logrus.Info("Kubeconfig changed, exiting to trigger a restart")
		interrupts.Terminate()
	}); err != nil {
		logrus.WithError(err).Fatal("Failed to register kubeconfig change callback")
	}

	opts := manager.Options{
		MetricsBindAddress:            "0",
		Namespace:                     cfg().ProwJobNamespace,
		LeaderElection:                true,
		LeaderElectionNamespace:       configAgent.Config().ProwJobNamespace,
		LeaderElectionID:              "prow-sinker-leaderlock",
		LeaderElectionReleaseOnCancel: true,
	}
	mgr, err := manager.New(infrastructureClusterConfig, opts)
	if err != nil {
		logrus.WithError(err).Fatal("Error creating manager")
	}

	buildManagers, err := o.kubernetes.BuildClusterManagers(o.dryRun.Value,
		func(o *manager.Options) {
			o.Namespace = cfg().PodNamespace
		},
	)
	if err != nil {
		logrus.WithError(err).Error("Failed to construct build cluster managers. Is there a bad entry in the kubeconfig secret?")
	}

	buildClusterClients := map[string]ctrlruntimeclient.Client{}
	for clusterName, buildManager := range buildManagers {
		if err := mgr.Add(buildManager); err != nil {
			logrus.WithError(err).Fatal("Failed to add build cluster manager to main manager")
		}
		buildClusterClients[clusterName] = buildManager.GetClient()
	}

	c := controller{
		ctx:           context.Background(),
		logger:        logrus.NewEntry(logrus.StandardLogger()),
		prowJobClient: mgr.GetClient(),
		podClients:    buildClusterClients,
		config:        cfg,
		runOnce:       o.runOnce,
	}
	if err := mgr.Add(&c); err != nil {
		logrus.WithError(err).Fatal("failed to add controller to manager")
	}
	if err := mgr.Start(interrupts.Context()); err != nil {
		logrus.WithError(err).Fatal("failed to start manager")
	}
	logrus.Info("Manager ended gracefully")
}

type controller struct {
	ctx           context.Context
	cancel        context.CancelFunc
	logger        *logrus.Entry
	prowJobClient ctrlruntimeclient.Client
	podClients    map[string]ctrlruntimeclient.Client
	config        config.Getter
	runOnce       bool
	// cleanDuplicates makes sinker delete all duplicate jobs
	// that are completed and not a periodic. Jobs are deemed
	// a duplicate if their .spec.job, .spec.refs, .spec.extra_refs
	// and .status.state fields are identical. Keeping these
	// doesn't provide any value.
	cleanDuplicates bool
}

func (c *controller) Start(ctx context.Context) error {
	runChan := make(chan struct{})

	// We want to be able to dynamically adjust to changed config values, hence we cant use a time.Ticker
	go func() {
		for {
			runChan <- struct{}{}
			time.Sleep(c.config().Sinker.ResyncPeriod.Duration)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("stop signal received, quitting")
			return nil
		case <-runChan:
			start := time.Now()
			c.clean()
			c.logger.Infof("Sync time: %v", time.Since(start))
			if c.runOnce {
				return nil
			}
		}
	}
}

type sinkerReconciliationMetrics struct {
	podsCreated            int
	startAt                time.Time
	finishedAt             time.Time
	podsRemoved            map[string]int
	podRemovalErrors       map[string]int
	prowJobsCreated        int
	prowJobsCleaned        map[string]int
	prowJobsCleaningErrors map[string]int
}

// Prometheus Metrics
var (
	sinkerMetrics = struct {
		podsCreated            prometheus.Gauge
		timeUsed               prometheus.Gauge
		podsRemoved            *prometheus.GaugeVec
		podRemovalErrors       *prometheus.GaugeVec
		prowJobsCreated        prometheus.Gauge
		prowJobsCleaned        *prometheus.GaugeVec
		prowJobsCleaningErrors *prometheus.GaugeVec
	}{
		podsCreated: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sinker_pods_existing",
			Help: "Number of the existing pods in each sinker cleaning.",
		}),
		timeUsed: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sinker_loop_duration_seconds",
			Help: "Time used in each sinker cleaning.",
		}),
		podsRemoved: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sinker_pods_removed",
			Help: "Number of pods removed in each sinker cleaning.",
		}, []string{
			"reason",
		}),
		podRemovalErrors: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sinker_pod_removal_errors",
			Help: "Number of errors which occurred in each sinker pod cleaning.",
		}, []string{
			"reason",
		}),
		prowJobsCreated: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sinker_prow_jobs_existing",
			Help: "Number of the existing prow jobs in each sinker cleaning.",
		}),
		prowJobsCleaned: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sinker_prow_jobs_cleaned",
			Help: "Number of prow jobs cleaned in each sinker cleaning.",
		}, []string{
			"reason",
		}),
		prowJobsCleaningErrors: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sinker_prow_jobs_cleaning_errors",
			Help: "Number of errors which occurred in each sinker prow job cleaning.",
		}, []string{
			"reason",
		}),
	}
)

func init() {
	prometheus.MustRegister(sinkerMetrics.podsCreated)
	prometheus.MustRegister(sinkerMetrics.timeUsed)
	prometheus.MustRegister(sinkerMetrics.podsRemoved)
	prometheus.MustRegister(sinkerMetrics.podRemovalErrors)
	prometheus.MustRegister(sinkerMetrics.prowJobsCreated)
	prometheus.MustRegister(sinkerMetrics.prowJobsCleaned)
	prometheus.MustRegister(sinkerMetrics.prowJobsCleaningErrors)
}

func (m *sinkerReconciliationMetrics) getTimeUsed() time.Duration {
	return m.finishedAt.Sub(m.startAt)
}

func (c *controller) clean() {

	metrics := sinkerReconciliationMetrics{
		startAt:                time.Now(),
		podsRemoved:            map[string]int{},
		podRemovalErrors:       map[string]int{},
		prowJobsCleaned:        map[string]int{},
		prowJobsCleaningErrors: map[string]int{}}

	// Clean up old prow jobs first.
	prowJobs := &prowapi.ProwJobList{}
	if err := c.prowJobClient.List(c.ctx, prowJobs, ctrlruntimeclient.InNamespace(c.config().ProwJobNamespace)); err != nil {
		c.logger.WithError(err).Error("Error listing prow jobs.")
		return
	}
	metrics.prowJobsCreated = len(prowJobs.Items)

	// Only delete pod if its prowjob is marked as finished
	pjMap := make(map[string]*prowapi.ProwJob, len(prowJobs.Items))
	isFinished := sets.NewString()

	// Sort by creationTimestamp so we always keep the youngest if we deduplicate jobs.
	// We must sort before we construct the pjMap, otherwise the pjMap will be wrong
	// if the sorting swapped anything. This is because its values are pointers pointing
	// to elements in prowjob.Items which is a slice of values and those values are
	// changed when swapping.
	sort.Slice(prowJobs.Items, func(i, j int) bool {
		return prowJobs.Items[i].CreationTimestamp.After(prowJobs.Items[j].CreationTimestamp.Time)
	})

	maxProwJobAge := c.config().Sinker.MaxProwJobAge.Duration
	for i, prowJob := range prowJobs.Items {
		pjMap[prowJob.ObjectMeta.Name] = &prowJobs.Items[i]
		// Handle periodics separately.
		if prowJob.Spec.Type == prowapi.PeriodicJob {
			continue
		}
		if !prowJob.Complete() {
			continue
		}
		isFinished.Insert(prowJob.ObjectMeta.Name)
		if time.Since(prowJob.Status.StartTime.Time) <= maxProwJobAge {
			continue
		}
		if err := c.prowJobClient.Delete(c.ctx, &prowJob); err == nil {
			c.logger.WithFields(pjutil.ProwJobFields(&prowJob)).Info("Deleted prowjob.")
			metrics.prowJobsCleaned[reasonProwJobAged]++
		} else {
			c.logger.WithFields(pjutil.ProwJobFields(&prowJob)).WithError(err).Error("Error deleting prowjob.")
			metrics.prowJobsCleaningErrors[string(k8serrors.ReasonForError(err))]++
		}
	}

	if c.cleanDuplicates {
		duplicates, err := getDuplicates(c.logger, prowJobs)
		if err != nil {
			c.logger.WithError(err).Error("Failed to get duplicates")
		} else {
			for _, duplicateName := range duplicates {
				if err := c.prowJobClient.Delete(c.ctx, pjMap[duplicateName]); err == nil {
					c.logger.WithFields(pjutil.ProwJobFields(pjMap[duplicateName])).Info("Deleted duplicate prowjob.")
					metrics.prowJobsCleaned[reasonDuplicate]++
				} else {
					c.logger.WithFields(pjutil.ProwJobFields(pjMap[duplicateName])).WithError(err).Error("Error deleting duplicate prowjob.")
					metrics.prowJobsCleaningErrors[string(k8serrors.ReasonForError(err))]++
				}
			}
		}
	}

	// Keep track of what periodic jobs are in the config so we will
	// not clean up their last prowjob.
	isActivePeriodic := make(map[string]bool)
	for _, p := range c.config().Periodics {
		isActivePeriodic[p.Name] = true
	}

	// Get the jobs that we need to retain so horologium can continue working
	// as intended.
	latestPeriodics := pjutil.GetLatestProwJobs(prowJobs.Items, prowapi.PeriodicJob)
	for _, prowJob := range prowJobs.Items {
		if prowJob.Spec.Type != prowapi.PeriodicJob {
			continue
		}

		latestPJ := latestPeriodics[prowJob.Spec.Job]
		if isActivePeriodic[prowJob.Spec.Job] && prowJob.ObjectMeta.Name == latestPJ.ObjectMeta.Name {
			// Ignore deleting this one.
			continue
		}
		if !prowJob.Complete() {
			continue
		}
		isFinished.Insert(prowJob.ObjectMeta.Name)
		if time.Since(prowJob.Status.StartTime.Time) <= maxProwJobAge {
			continue
		}
		if err := c.prowJobClient.Delete(c.ctx, &prowJob); err == nil {
			c.logger.WithFields(pjutil.ProwJobFields(&prowJob)).Info("Deleted prowjob.")
			metrics.prowJobsCleaned[reasonProwJobAgedPeriodic]++
		} else {
			c.logger.WithFields(pjutil.ProwJobFields(&prowJob)).WithError(err).Error("Error deleting prowjob.")
			metrics.prowJobsCleaningErrors[string(k8serrors.ReasonForError(err))]++
		}
	}

	// Now clean up old pods.
	for cluster, client := range c.podClients {
		log := c.logger.WithField("cluster", cluster)
		var isClusterExcluded bool
		for _, excludeCluster := range c.config().Sinker.ExcludeClusters {
			if excludeCluster == cluster {
				isClusterExcluded = true
				break
			}
		}
		if isClusterExcluded {
			log.Debugf("Cluster %q is excluded, skipping pods deletion.", cluster)
			continue
		}
		var pods corev1api.PodList
		if err := client.List(c.ctx, &pods, ctrlruntimeclient.MatchingLabels{kube.CreatedByProw: "true"}, ctrlruntimeclient.InNamespace(c.config().PodNamespace)); err != nil {
			log.WithError(err).Error("Error listing pods.")
			continue
		}
		log.WithField("pod-count", len(pods.Items)).Debug("Successfully listed pods.")
		metrics.podsCreated += len(pods.Items)
		maxPodAge := c.config().Sinker.MaxPodAge.Duration
		terminatedPodTTL := c.config().Sinker.TerminatedPodTTL.Duration
		for _, pod := range pods.Items {
			reason := ""
			clean := false

			// by default, use the pod name as the key to match the associated prow job
			// this is to support legacy plank in case the kube.ProwJobIDLabel label is not set
			podJobName := pod.ObjectMeta.Name
			// if the pod has the kube.ProwJobIDLabel label, use this instead of the pod name
			if value, ok := pod.ObjectMeta.Labels[kube.ProwJobIDLabel]; ok {
				podJobName = value
			}
			log = log.WithField("pj", podJobName)
			terminationTime := time.Time{}
			if pj, ok := pjMap[podJobName]; ok && pj.Complete() {
				terminationTime = pj.Status.CompletionTime.Time
			}

			if podNeedsKubernetesFinalizerCleanup(log, pjMap[podJobName], &pod) {
				if err := c.cleanupKubernetesFinalizer(&pod, client); err != nil {
					log.WithError(err).Error("Failed to remove kubernetesreporter finalizer")
				}
			}

			switch {
			case !pod.Status.StartTime.IsZero() && time.Since(pod.Status.StartTime.Time) > maxPodAge:
				clean = true
				reason = reasonPodAged
			case !terminationTime.IsZero() && time.Since(terminationTime) > terminatedPodTTL:
				clean = true
				reason = reasonPodTTLed
			}

			if !isFinished.Has(podJobName) {
				// prowjob exists and is not marked as completed yet
				// deleting the pod now will result in plank creating a brand new pod
				clean = false
			}

			if c.isPodOrphaned(log, &pod, podJobName) {
				// prowjob has gone, we want to clean orphan pods regardless of the state
				reason = reasonPodOrphaned
				clean = true
			}

			if !clean {
				continue
			}

			c.deletePod(log, &pod, reason, client, &metrics)
		}
	}

	metrics.finishedAt = time.Now()
	sinkerMetrics.podsCreated.Set(float64(metrics.podsCreated))
	sinkerMetrics.timeUsed.Set(float64(metrics.getTimeUsed().Seconds()))
	for k, v := range metrics.podsRemoved {
		sinkerMetrics.podsRemoved.WithLabelValues(k).Set(float64(v))
	}
	for k, v := range metrics.podRemovalErrors {
		sinkerMetrics.podRemovalErrors.WithLabelValues(k).Set(float64(v))
	}
	sinkerMetrics.prowJobsCreated.Set(float64(metrics.prowJobsCreated))
	for k, v := range metrics.prowJobsCleaned {
		sinkerMetrics.prowJobsCleaned.WithLabelValues(k).Set(float64(v))
	}
	for k, v := range metrics.prowJobsCleaningErrors {
		sinkerMetrics.prowJobsCleaningErrors.WithLabelValues(k).Set(float64(v))
	}
	version.GatherProwVersion(c.logger)
	c.logger.Info("Sinker reconciliation complete.")
}

func (c *controller) cleanupKubernetesFinalizer(pod *corev1api.Pod, client ctrlruntimeclient.Client) error {

	oldPod := pod.DeepCopy()
	pod.Finalizers = sets.NewString(pod.Finalizers...).Delete(kubernetesreporterapi.FinalizerName).List()

	if err := client.Patch(c.ctx, pod, ctrlruntimeclient.MergeFrom(oldPod)); err != nil {
		return fmt.Errorf("failed to patch pod: %w", err)
	}

	return nil
}

func (c *controller) deletePod(log *logrus.Entry, pod *corev1api.Pod, reason string, client ctrlruntimeclient.Client, m *sinkerReconciliationMetrics) {
	name := pod.Name
	// Delete old finished or orphan pods. Don't quit if we fail to delete one.
	if err := client.Delete(c.ctx, pod); err == nil {
		log.WithFields(logrus.Fields{"pod": name, "reason": reason}).Info("Deleted old completed pod.")
		m.podsRemoved[reason]++
	} else {
		m.podRemovalErrors[string(k8serrors.ReasonForError(err))]++
		if k8serrors.IsNotFound(err) {
			log.WithField("pod", name).WithError(err).Info("Could not delete missing pod.")
		} else {
			log.WithField("pod", name).WithError(err).Error("Error deleting pod.")
		}
	}
}

func (c *controller) isPodOrphaned(log *logrus.Entry, pod *corev1api.Pod, prowJobName string) bool {
	// ProwJobs are cached and the cache may lag a bit behind, so never considers
	// pods that are less than 30 seconds old as orphaned
	if !pod.CreationTimestamp.Before(&metav1.Time{Time: time.Now().Add(-30 * time.Second)}) {
		return false
	}

	// We do a list in the very beginning of our processing. By the time we reach this check, that
	// list might be outdated, so do another GET here before declaring the pod orphaned
	pjName := types.NamespacedName{Namespace: c.config().ProwJobNamespace, Name: prowJobName}
	if err := c.prowJobClient.Get(c.ctx, pjName, &prowapi.ProwJob{}); err != nil {
		if k8serrors.IsNotFound(err) {
			return true
		}
		logrus.WithError(err).Error("Failed to get prowjob")
	}

	return false
}

func podNeedsKubernetesFinalizerCleanup(log *logrus.Entry, pj *prowapi.ProwJob, pod *corev1api.Pod) bool {
	// Can happen if someone deletes the prowjob before it finishes
	if pj == nil {
		return true
	}
	// This is always a bug
	if pj.Complete() && pj.Status.PrevReportStates[kubernetesreporterapi.ReporterName] == pj.Status.State && sets.NewString(pod.Finalizers...).Has(kubernetesreporterapi.FinalizerName) {
		log.WithField("pj", pj.Name).Errorf("BUG: Pod for prowjob still had the %s finalizer after completing and being successfully reported by the %s reporter", kubernetesreporterapi.FinalizerName, kubernetesreporterapi.ReporterName)

		return true
	}

	return false
}

func getDuplicates(l *logrus.Entry, pjs *prowapi.ProwJobList) (result []string, err error) {
	start := time.Now()
	defer func() {
		l.WithField("num_duplicates_found", len(result)).WithField("getDuplicates duration", time.Since(start)).Debug("getDuplicates finished")
	}()
	type duplicationKey struct {
		job string
		// pointer types are only equal
		// when they point to the same
		// address, so we have to serialize
		// the value of these fields
		refs      string
		extraRefs string
		state     prowapi.ProwJobState
	}
	duplicates := map[duplicationKey][]string{}

	var key duplicationKey
	for _, pj := range pjs.Items {
		if pj.Spec.Type == prowapi.PeriodicJob || !pj.Complete() {
			continue
		}

		key.job = pj.Spec.Job
		key.refs, err = comparableRefsString(pj.Spec.Refs)
		if err != nil {
			return nil, err
		}
		key.extraRefs, err = comparableExtraRefsString(pj.Spec.ExtraRefs)
		if err != nil {
			return nil, err
		}
		key.state = pj.Status.State

		duplicates[key] = append(duplicates[key], pj.Name)
	}

	for _, duplicateSlice := range duplicates {
		if len(duplicateSlice) == 1 {
			continue
		}
		result = append(result, duplicateSlice[1:]...)
	}

	return result, nil
}

func comparableExtraRefsString(extra []prowapi.Refs) (string, error) {
	if len(extra) == 0 {
		return "", nil
	}
	if len(extra) == 1 {
		return comparableRefsString(&extra[0])
	}
	b := &strings.Builder{}
	for _, ref := range extra {
		res, err := comparableRefsString(&ref)
		if err != nil {
			return "", err
		}
		b.WriteString(res)
	}

	return b.String(), nil
}

func comparableRefsString(r *prowapi.Refs) (string, error) {
	if r == nil {
		return "", nil
	}
	result, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return string(result), err
}
