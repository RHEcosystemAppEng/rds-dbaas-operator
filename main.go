/*
Copyright 2022.

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
	"fmt"
	"os"
	"time"

	"go.uber.org/zap/zapcore"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dbaasv1beta1 "github.com/RHEcosystemAppEng/dbaas-operator/api/v1beta1"
	rdsdbaasv1alpha1 "github.com/RHEcosystemAppEng/rds-dbaas-operator/api/v1alpha1"
	"github.com/RHEcosystemAppEng/rds-dbaas-operator/controllers"
	controllersrds "github.com/RHEcosystemAppEng/rds-dbaas-operator/controllers/rds"
	rdsv1alpha1 "github.com/aws-controllers-k8s/rds-controller/apis/v1alpha1"
	ackv1alpha1 "github.com/aws-controllers-k8s/runtime/apis/core/v1alpha1"
	//+kubebuilder:scaffold:imports
)

const (
	InstallNamespaceEnvVar = "INSTALL_NAMESPACE"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(rdsdbaasv1alpha1.AddToScheme(scheme))
	utilruntime.Must(dbaasv1beta1.AddToScheme(scheme))
	utilruntime.Must(rdsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(ackv1alpha1.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var syncPeriod time.Duration
	var logLevel string
	var rdsControllerRetries int
	var rdsControllerInterval time.Duration
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.DurationVar(&syncPeriod, "sync-period-min", 180*time.Minute, "The minimum interval at which watched resources are reconciled (e.g. 30 minutes).")
	flag.StringVar(&logLevel, "log-level", "info", "Log level.")
	flag.IntVar(&rdsControllerRetries, "wait-for-rds-controller-retries", 15, "The maximum times to check if the RDS controller is ready to run before setting up the Inventory controller.")
	flag.DurationVar(&rdsControllerInterval, "wait-for-rds-controller-interval", 30*time.Second, "The interval at which to check if the RDS controller is ready to run before setting up the Inventory controller.")

	var level zapcore.Level
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		//default to info level
		level = zapcore.InfoLevel
	}

	opts := zap.Options{
		Development: true,
		Level:       level,
		TimeEncoder: zapcore.RFC3339TimeEncoder,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "47bdf935.redhat.com",
		SyncPeriod:             &syncPeriod,
		NewCache: cache.BuilderWithOptions(cache.Options{
			SelectorsByObject: cache.SelectorsByObject{
				&v1.Secret{}: {
					Label: labels.SelectorFromSet(labels.Set{
						dbaasv1beta1.TypeLabelKey: dbaasv1beta1.TypeLabelValue,
					}),
				},
				&v1.ConfigMap{}: {
					Label: labels.SelectorFromSet(labels.Set{
						dbaasv1beta1.TypeLabelKey: dbaasv1beta1.TypeLabelValue,
					}),
				},
			},
		}),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	cfg := mgr.GetConfig()
	clientSet, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "unable to create clientset")
		os.Exit(1)
	}

	installNamespace, err := getInstallNamespace()
	if err != nil {
		setupLog.Error(err, "unable to retrieve install namespace")
	}

	if err = (&controllers.RDSInventoryReconciler{
		Client:                             mgr.GetClient(),
		Scheme:                             mgr.GetScheme(),
		GetDescribeDBInstancesPaginatorAPI: controllersrds.NewDescribeDBInstancesPaginator,
		GetModifyDBInstanceAPI:             controllersrds.NewModifyDBInstance,
		GetDescribeDBInstancesAPI:          controllersrds.NewDescribeDBInstances,
		GetDescribeDBClustersPaginatorAPI:  controllersrds.NewDescribeDBClustersPaginator,
		GetModifyDBClusterAPI:              controllersrds.NewModifyDBCluster,
		GetDescribeDBClustersAPI:           controllersrds.NewDescribeDBClusters,
		ACKInstallNamespace:                installNamespace,
		WaitForRDSControllerInterval:       rdsControllerInterval,
		WaitForRDSControllerRetries:        rdsControllerRetries,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RDSInventory")
		os.Exit(1)
	}
	if err = (&controllers.RDSConnectionReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RDSConnection")
		os.Exit(1)
	}
	if err = (&controllers.RDSInstanceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RDSInstance")
		os.Exit(1)
	}
	if err = (&controllers.DBaaSProviderReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Clientset: clientSet,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DBaaSProvider")
		os.Exit(1)
	}

	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err = (&rdsdbaasv1alpha1.RDSInventory{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "RDSInventory")
			os.Exit(1)
		}
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func getInstallNamespace() (string, error) {
	ns, found := os.LookupEnv(InstallNamespaceEnvVar)
	if !found {
		return "", fmt.Errorf("%s must be set", InstallNamespaceEnvVar)
	}
	return ns, nil
}
