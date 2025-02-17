// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DataDog/chaos-controller/cgroup"
	"github.com/DataDog/chaos-controller/container"
	"github.com/DataDog/chaos-controller/env"
	"github.com/DataDog/chaos-controller/injector"
	logger "github.com/DataDog/chaos-controller/log"
	"github.com/DataDog/chaos-controller/metrics"
	"github.com/DataDog/chaos-controller/metrics/types"
	"github.com/DataDog/chaos-controller/netns"
	"github.com/DataDog/chaos-controller/network"
	chaostypes "github.com/DataDog/chaos-controller/types"
	"github.com/cenkalti/backoff"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	readinessProbeFile = "/tmp/readiness_probe"
)

var rootCmd = &cobra.Command{
	Use:               "chaos-injector",
	Short:             "Datadog chaos failures injection application",
	Run:               nil,
	PersistentPostRun: cleanAndExit,
}

var (
	log                  *zap.SugaredLogger
	dryRun               bool
	ms                   metrics.Sink
	sink                 string
	level                string
	targetContainerIDs   []string
	targetPodIP          string
	disruptionName       string
	disruptionNamespace  string
	chaosNamespace       string
	targetName           string
	targetNodeName       string
	onInit               bool
	pulseActiveDuration  time.Duration
	pulseDormantDuration time.Duration
	deadlineRaw          string
	handlerPID           uint32
	configs              []injector.Config
	signals              chan os.Signal
	injectors            []injector.Injector
	readyToInject        bool
	dnsServer            string
	kubeDNS              string
)

func init() {
	rootCmd.AddCommand(networkDisruptionCmd)
	rootCmd.AddCommand(nodeFailureCmd)
	rootCmd.AddCommand(containerFailureCmd)
	rootCmd.AddCommand(cpuPressureCmd)
	rootCmd.AddCommand(diskPressureCmd)
	rootCmd.AddCommand(dnsDisruptionCmd)
	rootCmd.AddCommand(grpcDisruptionCmd)

	// basic args
	rootCmd.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "Enable dry-run mode")
	rootCmd.PersistentFlags().StringVar(&sink, "metrics-sink", "noop", "Metrics sink (datadog, or noop)")
	rootCmd.PersistentFlags().StringVar(&level, "level", "", "Level of injection (either pod or node)")
	rootCmd.PersistentFlags().StringSliceVar(&targetContainerIDs, "target-container-ids", []string{}, "Targeted containers ID")
	rootCmd.PersistentFlags().StringVar(&targetPodIP, "target-pod-ip", "", "Pod IP of targeted pod")
	rootCmd.PersistentFlags().BoolVar(&onInit, "on-init", false, "Apply the disruption on initialization, requiring a synchronization with the chaos-handler container")
	rootCmd.PersistentFlags().DurationVar(&pulseActiveDuration, "pulse-active-duration", time.Duration(0), "Duration of the disruption being active in a pulsing disruption (empty if the disruption is not pulsing)")
	rootCmd.PersistentFlags().DurationVar(&pulseDormantDuration, "pulse-dormant-duration", time.Duration(0), "Duration of the disruption being dormant in a pulsing disruption (empty if the disruption is not pulsing)")
	rootCmd.PersistentFlags().StringVar(&deadlineRaw, "deadline", "", "Timestamp at which the disruption must be over by")
	rootCmd.PersistentFlags().StringVar(&dnsServer, "dns-server", "8.8.8.8", "IP address of the upstream DNS server")
	rootCmd.PersistentFlags().StringVar(&kubeDNS, "kube-dns", "off", "Whether to use kube-dns for DNS resolution (off, internal, all)")
	rootCmd.PersistentFlags().StringVar(&chaosNamespace, "chaos-namespace", "chaos-engineering", "Namespace that contains this chaos pod")

	// log context args
	rootCmd.PersistentFlags().StringVar(&disruptionName, "log-context-disruption-name", "", "Log value: current disruption name")
	rootCmd.PersistentFlags().StringVar(&disruptionNamespace, "log-context-disruption-namespace", "", "Log value: current disruption namespace")
	rootCmd.PersistentFlags().StringVar(&targetName, "log-context-target-name", "", "Log value: current target name")
	rootCmd.PersistentFlags().StringVar(&targetNodeName, "log-context-target-node-name", "", "Log value: node hosting the current target pod")

	_ = cobra.MarkFlagRequired(rootCmd.PersistentFlags(), "level")
	cobra.OnInitialize(initLogger)
	cobra.OnInitialize(initMetricsSink)
	cobra.OnInitialize(initConfig)
	cobra.OnInitialize(initExitSignalsHandler)
}

func main() {
	// handle metrics sink client close on exit
	defer func() {
		log.Infow("closing metrics sink client before exiting", "sink", ms.GetSinkName())

		if err := ms.Close(); err != nil {
			log.Errorw("error closing metrics sink client", "error", err, "sink", ms.GetSinkName())
		}
	}()

	// execute command
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1) //nolint:gocritic
	}
}

// initLogger initializes a zap logger
func initLogger() {
	var err error

	// prepare logger
	log, err = logger.NewZapLogger()
	if err != nil {
		fmt.Printf("error while creating logger: %v", err)
		os.Exit(2)
	}

	log = log.With(
		"disruptionName", disruptionName,
		"disruptionNamespace", disruptionNamespace,
		"targetName", targetName,
		"targetNodeName", targetNodeName,
	)
}

// initMetricsSink initializes a metrics sink depending on the given flag
func initMetricsSink() {
	var err error

	ms, err = metrics.GetSink(types.SinkDriver(sink), types.SinkAppInjector)
	if err != nil {
		log.Errorw("error while creating metric sink, switching to noop sink", "error", err)

		ms, _ = metrics.GetSink(types.SinkDriverNoop, types.SinkAppInjector)
	}
}

// initConfig initializes the injector config and main components from the given flags
func initConfig() {
	pids := []uint32{}
	ctns := []container.Container{}
	dnsConfig := network.DNSConfig{DNSServer: dnsServer, KubeDNS: kubeDNS}

	// log when dry-run mode is enabled
	if dryRun {
		log.Warn("dry run mode enabled, no disruption will be injected but most of the commands will still be executed to simulate it as much as possible")
	}

	switch level {
	case chaostypes.DisruptionLevelPod:
		// check for container ID flag
		if len(targetContainerIDs) == 0 {
			log.Error("--target-container-ids flag must be passed when --level=pod")

			return
		}

		for _, containerID := range targetContainerIDs {
			// retrieve container info
			ctn, err := container.New(containerID)
			if err != nil {
				log.Errorw("can't create container object", "error", err)

				return
			}

			log.Infow("injector targeting container", "containerID", containerID, "container name", ctn.Name())

			pid := ctn.PID()

			// keep pid for later if this is a chaos handler container
			if onInit && ctn.Name() == "chaos-handler" {
				handlerPID = pid
			}

			ctns = append(ctns, ctn)
			pids = append(pids, pid)
		}

		// check for pod IP flag
		if targetPodIP == "" {
			log.Error("--target-pod-ip flag must be passed when --level=pod")

			return
		}

	case chaostypes.DisruptionLevelNode:
		pids = []uint32{1}
		ctns = []container.Container{nil}
	default:
		log.Errorf("unknown level: %s", level)

		return
	}

	// create kubernetes clientset
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Errorw("error getting kubernetes client config", "error", err)

		return
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Errorw("error creating kubernetes client", "error", err)

		return
	}

	// create injector configs
	for i, pid := range pids {
		// create network namespace manager
		netnsMgr, err := netns.NewManager(pid)
		if err != nil {
			log.Errorw("error creating network namespace manager", "error", err, "pid", pid)

			return
		}

		// create cgroups manager
		cgroupMgr, err := cgroup.NewManager(dryRun, pid, log)
		if err != nil {
			log.Errorw("error creating cgroup manager", "error", err, "pid", pid)

			return
		}

		// generate injector config
		config := injector.Config{
			DryRun:          dryRun,
			OnInit:          onInit,
			Log:             log,
			MetricsSink:     ms,
			Level:           chaostypes.DisruptionLevel(level),
			TargetContainer: ctns[i],
			TargetPodIP:     targetPodIP,
			TargetNodeName:  targetNodeName,
			Cgroup:          cgroupMgr,
			Netns:           netnsMgr,
			K8sClient:       clientset,
			DNS:             dnsConfig,
		}

		configs = append(configs, config)
	}

	// mark the disruption as ready to be injected only when all injector configurations are successfully created
	readyToInject = true
}

// initExitSignalsHandler initializes the exit signal handler
func initExitSignalsHandler() {
	// signals needs to be a buffered channel, so that we can receive a signal anytime during the injection process
	// as an unbuffered channel will ignore signals sent before it begins listening. We can keep the buffer size to `1` here
	// as no matter how many SIGINT or SIGTERMs we receive, we want to carry out the same action: clean and die
	signals = make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
}

// inject inject all the disruptions using the list of injectors
// returns true if injection succeeded, false otherwise
func inject(kind string, sendToMetrics bool) bool {
	errOnInject := false

	for _, inj := range injectors {
		// start injection, do not fatal on error so we keep the pod
		// running, allowing the cleanup to happen
		if err := inj.Inject(); err != nil {
			errOnInject = true

			if sendToMetrics {
				handleMetricError(ms.MetricInjected(false, kind, nil))
			}

			log.Errorw("disruption injection failed", "error", err)
		} else {
			if sendToMetrics {
				handleMetricError(ms.MetricInjected(true, kind, nil))
			}

			log.Infof("disruption %s injected", inj.GetDisruptionKind())
		}
	}

	if errOnInject {
		log.Errorf("an injector could not inject the disruption successfully, please look at the logs above for more details")
	}

	return !errOnInject
}

// clean clean all the disruptions using the list of injectors
// returns true if cleanup succeeded, false otherwise
func clean(kind string, sendToMetrics bool) bool {
	errOnClean := false

	for _, inj := range injectors {
		// start cleanup which is retried up to 3 times using an exponential backoff algorithm
		if err := backoff.RetryNotify(inj.Clean, backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 3), retryNotifyHandler); err != nil {
			errOnClean = true

			if sendToMetrics {
				handleMetricError(ms.MetricCleaned(false, kind, nil))
			}

			log.Errorw("disruption cleaning failed", "error", err)
		} else {
			log.Infof("disruption %s cleaned", inj.GetDisruptionKind())
		}

		if sendToMetrics {
			handleMetricError(ms.MetricCleaned(true, kind, nil))
		}
	}

	if errOnClean {
		log.Errorw("an injector could not clean the disruption successfully, please look at the logs above for more details")
	}

	return !errOnClean
}

// injectAndWait injects the disruption with the configured injector and waits
// for an exit signal to be sent
func injectAndWait(cmd *cobra.Command, args []string) {
	// early exit if an injector configuration failed to be generated during initialization
	if !readyToInject || len(injectors) == 0 {
		log.Error("an injector could not be configured successfully during initialization, aborting the injection now")

		return
	}

	log.Infow("injecting the disruption", "kind", cmd.Name())

	injectSuccess := inject(cmd.Name(), true)

	// create and write readiness probe file if injection succeeded so the pod is marked as ready
	if injectSuccess {
		log.Infof("disruption(s) injected, now waiting for an exit signal")

		if err := ioutil.WriteFile(readinessProbeFile, []byte("1"), 0400); err != nil {
			log.Errorw("error writing readiness probe file", "error", err)
		}
	}

	// once injected, send a signal to the handler container so it can exit and let other containers go on
	if onInit {
		log.Info("notifying the handler container that injection is now done")

		// ensure a handler container was found
		if handlerPID != 0 {
			// retrieve handler container process to send a signal
			handlerProcess, err := os.FindProcess(int(handlerPID))
			if err != nil {
				log.Errorw("error retrieving handler container process", "error", err)
			}

			// send the SIGUSR1 signal
			if err := handlerProcess.Signal(syscall.SIGUSR1); err != nil {
				log.Errorw("error sending a SIGUSR1 signal to the handler container process", "error", err, "pid", handlerPID)
			}
		} else {
			log.Error("the --on-init flag was provided but no handler container could be found")
		}
	}

	deadline, err := time.Parse(time.RFC3339, deadlineRaw)
	if err != nil {
		deadline = time.Now().Add(time.Hour)

		log.Errorw("unable to determine disruption deadline, will self-terminate in one hour instead", "err", err)
	}

	if injectSuccess && pulseActiveDuration > 0 && pulseDormantDuration > 0 {
		var action func(string, bool) bool

		isInjected := true
		sleepDuration := pulseActiveDuration

		for { // This loop will wait, clean, wait, inject until a signal is received
			select { // Quit on signal reception or sleep and injects / cleans the disruptions
			case sig := <-signals:
				log.Infow("an exit signal has been received", "signal", sig.String())

				return
			case <-time.After(getDuration(deadline)):
				log.Infow("duration has expired")

				return
			case <-time.After(sleepDuration):
				if !isInjected {
					action = inject
					sleepDuration = pulseDormantDuration
				} else {
					action = clean
					sleepDuration = pulseActiveDuration
				}

				if ok := action(cmd.Name(), false); !ok {
					return
				}

				isInjected = !isInjected
			}
		}
	}

	// wait for an exit signal, this is a blocking call
	select {
	case sig := <-signals:
		log.Infow("an exit signal has been received", "signal", sig.String())
	case <-time.After(getDuration(deadline)):
		log.Infow("duration has expired")
	}
}

// cleanAndExit cleans the disruption with the configured injector and exits nicely
func cleanAndExit(cmd *cobra.Command, args []string) {
	// 1 or more injectors failed to clean, we exit
	if ok := clean(cmd.Name(), true); !ok {
		os.Exit(1)
	}

	if err := backoff.RetryNotify(cleanFinalizer, backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 3), retryNotifyHandler); err != nil {
		log.Errorw("couldn't safely remove this pod's finalizer", "err", err)
	}

	log.Info("disruption(s) cleaned, now exiting")
}

func cleanFinalizer() error {
	if len(configs) == 0 {
		err := fmt.Errorf("no configuration available for this disruption")

		log.Warnw("couldn't GET this pod in order to remove its finalizer", "pod", os.Getenv(env.InjectorPodName), "err", err)

		return err
	}

	pod, err := configs[0].K8sClient.CoreV1().Pods(chaosNamespace).Get(context.Background(), os.Getenv(env.InjectorPodName), metav1.GetOptions{})
	if err != nil {
		log.Warnw("couldn't GET this pod in order to remove its finalizer", "pod", os.Getenv(env.InjectorPodName), "err", err)
		return err
	}

	controllerutil.RemoveFinalizer(pod, chaostypes.ChaosPodFinalizer)

	_, err = configs[0].K8sClient.CoreV1().Pods(chaosNamespace).Update(context.Background(), pod, metav1.UpdateOptions{})
	if err != nil {
		log.Warnw("couldn't remove this pod's finalizer", "pod", os.Getenv(env.InjectorPodName), "err", err)
		return err
	}

	return nil
}

// handleMetricError logs the given error if not nil
func handleMetricError(err error) {
	if err != nil {
		log.Errorw("error sending metric", "sink", ms.GetSinkName(), "error", err)
	}
}

// retryNotifyHandler is called when the cleanup fails
// it logs the error and the time to wait before the next retry
func retryNotifyHandler(err error, delay time.Duration) {
	log.Errorw("disruption cleanup failed", "error", err)
	log.Infof("retrying cleanup in %s", delay.String())
}

// getDuration returns the time between time.Now() and when the disruption is due to expire
// This gives the chaos pod plenty of time to clean up before it hits activeDeadlineSeconds and becomes Failed
func getDuration(deadline time.Time) time.Duration {
	return time.Until(deadline)
}
