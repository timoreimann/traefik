package integration

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/containous/traefik/integration/try"
	"github.com/containous/traefik/testhelpers"
	"github.com/go-check/check"
	checker "github.com/vdemeester/shakers"
	"golang.org/x/net/context"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	traefikNamespace          = "traefik"
	kubernetesVersion         = "v1.8.0"
	minikubeStartupTimeout    = 2 * time.Minute
	minikubeDeleteTimeout     = 30 * time.Second
	kubectlApplyTimeout       = 60 * time.Second
	examplesRelativeDirectory = "examples/k8s"
	envVarSkipVMCleanup       = "K8S_SKIP_CLEANUP"
	envVarMinikubeProfile     = "K8S_MINIKUBE_PROFILE"
)

var (
	minikubeProfile string
	kubeconfig      string
)

var (
	minikubeStartArgs = []string{
		"start",
		"--logtostderr",
		fmt.Sprintf("--kubernetes-version=%s", kubernetesVersion),
		"--extra-config=apiserver.Authorization.Mode=RBAC",
		"--keep-context",
		"--disk-size=15g",
	}

	minikubeEnvVars = []string{
		"MINIKUBE_WANTUPDATENOTIFICATION=false",
		"MINIKUBE_WANTREPORTERRORPROMPT=false",
		"MINIKUBE_WANTKUBECTLDOWNLOADMSG=false",
	}
)

type KubeConnection struct {
	client   *kubernetes.Clientset
	nodeHost string
	master   *url.URL
}

type KubernetesSuite struct {
	BaseSuite
	KubeConnection
	cleanupAfterCompletion bool
}

func (s *KubernetesSuite) SetUpSuite(c *check.C) {
	err := checkRequirements()
	c.Assert(err, checker.IsNil, check.Commentf("requirements failed: %s", err))

	err = setMinikubeParams()
	c.Assert(err, checker.IsNil, check.Commentf("failed to set minikube parameters: %s", err))

	onCI := os.Getenv("CI") != ""

	err = startMinikube(onCI)
	c.Assert(err, checker.IsNil, check.Commentf("failed to start minikube: %s", err))

	skipCleanup := os.Getenv(envVarSkipVMCleanup) != ""
	if !skipCleanup && !onCI {
		s.cleanupAfterCompletion = true
	}

	if !onCI {
		// Transfer current Traefik image into minikube. This is not necessary
		// on the CI where we use the host-based none driver.
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()
		fmt.Println("Transferring current Traefik image into minikube")
		err := runCommand(
			"bash",
			[]string{
				"-c",
				fmt.Sprintf("docker save containous/traefik:latest | (eval $(minikube docker-env --logtostderr -p %s) && docker load && docker tag containous/traefik:latest traefik:kube-test)", minikubeProfile),
			},
			commandParams{
				ctx: ctx,
				// SHELL needed by "minikube docker-env".
				envs: append(minikubeEnvVars, "SHELL=/bin/bash"),
			},
		)
		c.Assert(err, checker.IsNil)
	} else {
		err := runCommand("docker", []string{"tag", "containous/traefik:latest", "traefik:kube-test"}, commandParams{})
		c.Assert(err, checker.IsNil)
	}

	conn, err := createKubeConnection()
	c.Assert(err, checker.IsNil)
	s.KubeConnection = *conn

	fmt.Println("Waiting for cluster to become ready")
	err = try.Do(1*time.Minute, func() error {
		_, err := s.client.ServerVersion()
		return err
	})
	c.Assert(err, checker.IsNil)

	// fmt.Println("Wait another 15 seconds to be sure")
	// time.Sleep(15 * time.Second)
}

func (s *KubernetesSuite) TearDownSuite(c *check.C) {
	if s.cleanupAfterCompletion {
		fmt.Printf("Deleting minikube profile %q\n", minikubeProfile)
		ctx, cancel := context.WithTimeout(context.Background(), minikubeDeleteTimeout)
		defer cancel()

		err := runCommand("minikube",
			[]string{"delete", "--logtostderr"},
			commandParams{
				ctx:  ctx,
				envs: minikubeEnvVars,
			})
		c.Assert(err, checker.IsNil)

		fmt.Printf("Deleting kubectl context %q\n", minikubeProfile)
		err = runKubectl("config", "delete-context", minikubeProfile)
		c.Assert(err, checker.IsNil)
	}
}

func (s *KubernetesSuite) SetUpTest(c *check.C) {
	// (Re-)create the test namespace.

	// First, delete any left-overs from previous tests.
	policy := metav1.DeletePropagationForeground
	fmt.Printf("Deleting any left-overs of test namespace %q\n", traefikNamespace)
	grace := int64(0)
	err := s.client.CoreV1().Namespaces().Delete(traefikNamespace, &metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
		PropagationPolicy:  &policy,
	})
	// We ignore conflicts as they indicate that a deletion is already in
	// progress.
	if err != nil && !kerrors.IsConflict(err) && !kerrors.IsNotFound(err) {
		c.Fatalf("failed to delete namespace %q: %#+v", traefikNamespace, err)
	}

	// Retry creating because the preceding Delete operation takes a while to
	// complete, manifesting in 409 ("AlreadyExists") errors.
	fmt.Printf("Creating test namespace %q\n", traefikNamespace)
	err = try.Do(2*time.Minute, func() error {
		_, err := s.client.CoreV1().Namespaces().Create(&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: traefikNamespace,
			},
		})
		return err
	})

	c.Assert(err, checker.IsNil, check.Commentf("failed to create namespace %q: %s", traefikNamespace, err))
}

func (s *KubernetesSuite) TestDeploymentManifestExamples(c *check.C) {
	s.doTestManifestExamples(c, "traefik-deployment")
}

func (s *KubernetesSuite) TestDaemonSetManifestExamples(c *check.C) {
	s.doTestManifestExamples(c, "traefik-ds")
}

func (s *KubernetesSuite) doTestManifestExamples(c *check.C, workloadManifest string) {
	// Use Deployment manifest referencing current traefik binary.
	patchedDeployment := createAbsolutePath(fmt.Sprintf("integration/resources/k8s/%s.test.yaml", workloadManifest))

	// Validate Traefik is reachable.
	err := Apply(
		"traefik-rbac.yaml",
		patchedDeployment,
	)
	c.Assert(err, checker.IsNil)
	defer func() {
		if c.Failed() {
			fmt.Println("Traefik pod description:")
			runKubectl("describe", "pod", "-l", "k8s-app=traefik-ingress-lb")

			fmt.Println("Traefik pod logs:")
			runKubectl("logs", "deploy/traefik-ingress-controller")
		}
	}()

	// Get the service NodePort.
	svcName, err := getNameFromManifest(fmt.Sprintf("%s.yaml", workloadManifest), "Service")
	c.Assert(err, checker.IsNil, check.Commentf("failed to get service: %s", err))

	svc, err := s.client.CoreV1().Services(traefikNamespace).Get(svcName, metav1.GetOptions{})
	c.Assert(err, checker.IsNil)
	var nodePort int32
	for _, port := range svc.Spec.Ports {
		if port.Port == 80 {
			nodePort = port.NodePort
			break
		}
	}
	c.Assert(nodePort, checker.GreaterThan, int32(0), check.Commentf("failed to find NodePort matching port 80"))

	baseTraefikURL := fmt.Sprintf("http://%s:%d/", s.nodeHost, nodePort)
	err = try.GetRequest(baseTraefikURL, 45*time.Second, try.StatusCodeIs(http.StatusNotFound))
	c.Assert(err, checker.IsNil, check.Commentf("traefik access"))

	// Validate Traefik UI is reachable.
	err = Apply(
		"ui.yaml",
	)
	c.Assert(err, checker.IsNil)

	req := testhelpers.MustNewRequest(http.MethodGet, baseTraefikURL+"dashboard/", nil)
	req.Host = "traefik-ui.minikube"
	err = try.Request(req, 3*time.Second, try.StatusCodeIs(http.StatusOK))
	c.Assert(err, checker.IsNil, check.Commentf("traefik UI access (req: %s host: %s headers: %s)", *req, req.Host, req.Header))

	// Validate third-party service is routable through Traefik.
	err = Apply(
		"cheese-deployments.yaml",
		"cheese-services.yaml",
		"cheese-ingress.yaml",
	)
	c.Assert(err, checker.IsNil)

	req = testhelpers.MustNewRequest(http.MethodGet, baseTraefikURL, nil)
	for _, svc := range []string{"stilton", "cheddar", "wensleydale"} {
		req.Host = svc + ".minikube"
		err = try.Request(req, 25*time.Second, try.StatusCodeIs(http.StatusOK))
		c.Assert(err, checker.IsNil, check.Commentf("service %q access", svc))
	}
}

func checkRequirements() error {
	// TODO: Check for minimum versions.
	var missing []string
	minikubePath, err := exec.LookPath("minikube")
	if err != nil {
		missing = append(missing, "minikube")
	} else {
		runCommand(minikubePath, []string{"version"}, commandParams{})
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		missing = append(missing, "kubectl")
	}

	if len(missing) > 0 {
		return fmt.Errorf("the following components must be installed: %s", strings.Join(missing, ", "))
	}

	return nil
}

func setMinikubeParams() error {
	profile := os.Getenv(envVarMinikubeProfile)
	if profile == "" {
		t := time.Now().UTC()
		profile = fmt.Sprintf("traefik-integration-test-%d%02d%02dZ%02d%02d%02d",
			t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second())
	}
	fmt.Printf("Using minikube profile %q\n", profile)
	minikubeProfile = profile
	minikubeEnvVars = append(minikubeEnvVars, fmt.Sprintf("MINIKUBE_PROFILE=%s", minikubeProfile))

	minikubeHome := path.Join("/", os.Getenv("HOME"))
	if _, err := os.Stat(path.Join(minikubeHome, ".minikube")); err == nil {
		fmt.Printf("Using minikube home %q\n", minikubeHome)
		minikubeEnvVars = append(minikubeEnvVars, fmt.Sprintf("MINIKUBE_HOME=%s", minikubeHome))
	}

	kubeCfg := path.Join("/", os.Getenv("HOME"), ".kube", "config")
	if _, err := os.Stat(kubeCfg); err == nil {
		fmt.Printf("Using kubeconfig %q\n", kubeCfg)
		minikubeEnvVars = append(minikubeEnvVars, fmt.Sprintf("KUBECONFIG=%s", kubeCfg))
		kubeconfig = kubeCfg
	}

	vBoxManagePath, err := exec.LookPath("VBoxManage")
	if err != nil {
		return fmt.Errorf("cannot find VBoxManage path: %s", err)
	}
	minikubeEnvVars = append(minikubeEnvVars, fmt.Sprintf("PATH=%s", path.Dir(vBoxManagePath)))

	return nil
}

func startMinikube(onCI bool) error {
	// Check if minikube is already running.
	cmd := exec.Command("minikube", "status")
	cmd.Env = append(os.Environ(), minikubeEnvVars...)
	cmd.Stderr = os.Stderr
	fmt.Println("Checking minikube status")
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return fmt.Errorf("failed to determine minikube status: %s", err)
		}

		// Start minikube.

		minikubeInitCmd := "minikube"
		envVars := minikubeEnvVars
		// Adapt minikube parameters if we run on the CI system.
		if onCI {
			// Bootstrap Kubernetes natively on the host system.
			minikubeStartArgs = append(minikubeStartArgs, "--vm-driver=none")
			// Native Kubernetes requires root privileges.
			minikubeInitCmd = "sudo"
			minikubeStartArgs = append([]string{"--preserve-env", "minikube"}, minikubeStartArgs...)
			// Make sure root-owned files are moved to the proper location.
			envVars = append(minikubeEnvVars[:], "CHANGE_MINIKUBE_NONE_USER=true")
		}

		ctx, cancel := context.WithTimeout(context.Background(), minikubeStartupTimeout)
		defer cancel()
		fmt.Println("Starting minikube")
		return runCommand(minikubeInitCmd,
			minikubeStartArgs,
			// append(
			// 	minikubeStartArgs,
			// 	"-v=10",
			// ),
			commandParams{
				ctx:  ctx,
				envs: envVars,
			},
		)
	}

	return nil
}

type commandParams struct {
	ctx    context.Context
	envs   []string
	stdout io.Writer
}

func runCommand(cmd string, args []string, params commandParams) error {
	fmt.Printf("Starting: %s %s (env vars: %#+v)\n", cmd, args, params.envs)
	var c *exec.Cmd
	if params.ctx == nil {
		c = exec.Command(cmd, args...)
	} else {
		c = exec.CommandContext(params.ctx, cmd, args...)
	}
	c.Env = append(c.Env, params.envs...)

	if params.stdout == nil {
		params.stdout = os.Stdout
	}
	c.Stdout = params.stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func runKubectl(arguments ...string) error {
	var args []string
	if kubeconfig != "" {
		args = []string{"--kubeconfig", kubeconfig}
	}
	args = append(args, arguments...)

	ctx, cancel := context.WithTimeout(context.Background(), kubectlApplyTimeout)
	defer cancel()
	return runCommand("kubectl",
		append(
			[]string{
				"--context",
				minikubeProfile,
				"--namespace",
				traefikNamespace,
			},
			args...,
		),
		commandParams{ctx: ctx})
}

func createKubeConnection() (conn *KubeConnection, err error) {
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{
			CurrentContext: minikubeProfile,
			Context: clientcmdapi.Context{
				Namespace: traefikNamespace,
			},
		},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create configuration: %s", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %s", err)
	}

	master, err := url.Parse(config.Host)
	if err != nil {
		return nil, fmt.Errorf("failed to parse host URL %q: %s", config.Host, err)
	}

	return &KubeConnection{
		nodeHost: master.Hostname(),
		master:   master,
		client:   client,
	}, nil
}

func Apply(names ...string) error {
	for _, name := range names {
		manifest := name
		if !path.IsAbs(manifest) {
			manifest = createAbsoluteManifestPath(manifest)
		}

		if err := runKubectl("apply", "--filename", manifest); err != nil {
			return fmt.Errorf("failed to apply manifest %s: %s", manifest, err)
		}
	}

	return nil
}

func getNameFromManifest(name, kind string) (string, error) {
	manifest := createAbsoluteManifestPath(name)
	file, err := os.Open(manifest)
	if err != nil {
		return "", err
	}
	defer file.Close()

	var object struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
	}

	dec := yaml.NewYAMLToJSONDecoder(file)
	var numObjects int
	for {
		if err := dec.Decode(&object); err != nil {
			if err == io.EOF {
				return "", fmt.Errorf("failed to find object of kind %q among %d object(s) in manifest %q", kind, numObjects, name)
			}
			return "", fmt.Errorf("failed to decode manifest %q: %s", name, err)
		}

		numObjects++
		if object.Kind == kind {
			return object.Metadata.Name, nil
		}
	}
}

func createAbsoluteManifestPath(name string) string {
	return createAbsolutePath(filepath.Join(examplesRelativeDirectory, name))
}

func createAbsolutePath(relativeFileName string) string {
	cwd, err := os.Getwd()
	if err != nil {
		panic(fmt.Sprintf("failed to get current working directory: %s", err))
	}

	return filepath.Join(cwd, "..", relativeFileName)
}
