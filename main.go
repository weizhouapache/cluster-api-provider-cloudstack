/*
Copyright 2021.

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
	"flag"
	"os"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha4"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	infrav1 "gitlab.aws.dev/ce-pike/merida/cluster-api-provider-capc/api/v1alpha4"
	"gitlab.aws.dev/ce-pike/merida/cluster-api-provider-capc/controllers"
	"gitlab.aws.dev/ce-pike/merida/cluster-api-provider-capc/pkg/cloud"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(infrav1.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {

	// Parse args and setup logger.
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var watchingNamespace string
	flag.StringVar(&metricsAddr, "metrics-bind-address", "localhost:8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&watchingNamespace, "namespace", "", "Namespace that the controller watches to reconcile cluster-api objects. If unspecified, the controller watches for cluster-api objects across all namespaces.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true})))

	// Setup CloudStack api client.
	filepath := "/config/cloud-config"
	client, err := cloud.NewClient(filepath)
	if err != nil {
		setupLog.Error(err, "unable to start manager -- cannot connect to CloudStack via client")
		os.Exit(1)
	}
	setupLog.Info("CloudStack client initialized.")

	// Create the controller manager.
	mgr, err := ctrl.NewManager(config.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "capc-leader-election-controller",
		Namespace:              watchingNamespace,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Register machine and cluster reconcilers with the controller manager.
	ctx := ctrl.SetupSignalHandler()
	if err = (&controllers.CloudStackClusterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		CS:     client,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create CloudStack cluster controller")
		os.Exit(1)
	}
	if err = (&controllers.CloudStackMachineReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		CS:     client,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create CloudStack machine controller")
		os.Exit(1)
	}

	// Add health and ready checks.
	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to setup health check")
		os.Exit(1)
	}
	if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to setup ready check")
		os.Exit(1)
	}

	// Start the controller manager.
	if err = (&infrav1.CloudStackCluster{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "CloudStackCluster")
		os.Exit(1)
	}
	if err = (&infrav1.CloudStackMachine{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "CloudStackMachine")
		os.Exit(1)
	}
	if err = (&infrav1.CloudStackMachineTemplate{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "CloudStackMachineTemplate")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder
	setupLog.Info("starting controller manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "could not start the controller manager")
		os.Exit(1)
	}
}
