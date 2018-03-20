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
	minikubeStartupTimeout    = 90 * time.Second
	minikubeStopTimeout       = 30 * time.Second
	kubectlApplyTimeout       = 60 * time.Second
	examplesRelativeDirectory = "examples/k8s"
	envVarSkipVMCleanup       = "K8S_SKIP_VM_CLEANUP"
	envVarMinikubeProfile     = "K8S_MINIKUBE_PROFILE"
)

var minikubeProfile string

var (
	minikubeStartArgs = []string{
		"start",
		"--logtostderr",
		fmt.Sprintf("--kubernetes-version=%s", kubernetesVersion),
		"--extra-config=apiserver.Authorization.Mode=RBAC",
		"--keep-context",
		"--disk-size=15g",
		"--cache-images=false",
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
	stopAfterCompletion bool
}

func (s *KubernetesSuite) SetUpSuite(c *check.C) {
	err := checkRequirements()
	c.Assert(err, checker.IsNil, check.Commentf("requirements failed: %s", err))

	setMinikubeProfile()

	onCI := os.Getenv("CI") != ""

	err = startMinikube(onCI)
	c.Assert(err, checker.IsNil, check.Commentf("failed to start minikube: %s", err))

	skipStop := os.Getenv(envVarSkipVMCleanup) != ""
	if !skipStop && !onCI {
		s.stopAfterCompletion = true
	}

	if !onCI {
		// Transfer current Traefik image into minikube. This is not necessary
		// on the CI where we use the host-based none driver.
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()
		fmt.Println("Transferring current Traefik image into minikube")
		err := runCommandContext(ctx,
			"bash",
			[]string{
				"-c",
				fmt.Sprintf("docker save containous/traefik:latest | (eval $(minikube docker-env --alsologtostderr -p %s) && docker load && docker tag containous/traefik:latest traefik:kube-test)", minikubeProfile),
			},
			minikubeEnvVars)
		c.Assert(err, checker.IsNil)
	} else {
		err := runCommand("docker", []string{"tag", "containous/traefik:latest", "traefik:kube-test"}, nil)
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
	if s.stopAfterCompletion {
		fmt.Println("Stopping minikube as it was not running previously")
		ctx, cancel := context.WithTimeout(context.Background(), minikubeStopTimeout)
		defer cancel()

		err := runCommandContext(ctx,
			"minikube",
			[]string{"stop", "--logtostderr"},
			nil)
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
	err = try.Do(90*time.Second, func() error {
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
			runCommand("kubectl",
				[]string{
					"--context",
					minikubeProfile,
					"--namespace",
					traefikNamespace,
					"describe",
					"pod",
					"-l",
					"k8s-app=traefik-ingress-lb",
				},
				nil)

			fmt.Println("Traefik pod logs:")
			runCommand("kubectl",
				[]string{
					"--context",
					minikubeProfile,
					"--namespace",
					traefikNamespace,
					"logs",
					"deploy/traefik-ingress-controller",
				},
				nil)
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
	if _, err := exec.LookPath("minikube"); err != nil {
		missing = append(missing, "minikube")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		missing = append(missing, "kubectl")
	}

	if len(missing) > 0 {
		return fmt.Errorf("the following components must be installed: %s", strings.Join(missing, ", "))
	}

	return nil
}

func setMinikubeProfile() {
	profile := os.Getenv(envVarMinikubeProfile)
	if profile == "" {
		t := time.Now().UTC()
		profile = fmt.Sprintf("traefik-integration-test-%d%02d%02dZ%02d%02d%02d",
			t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second())
	}
	fmt.Printf("Using minikube profile %q\n", profile)
	minikubeProfile = profile
	minikubeEnvVars = append(minikubeEnvVars, fmt.Sprintf("MINIKUBE_PROFILE=%s", minikubeProfile))

	minikubeHome := path.Join("/", os.Getenv("HOME"), ".minikube")
	if _, err := os.Stat(minikubeHome); err == nil {
		fmt.Printf("Using minikube home %q\n", minikubeHome)
		minikubeEnvVars = append(minikubeEnvVars, fmt.Sprintf("MINIKUBE_HOME=%s", minikubeHome))
	}
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

		vBoxManagePath, err := exec.LookPath("VBoxManage")
		if err != nil {
			return fmt.Errorf("cannot find VBoxManage path: %s", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), minikubeStartupTimeout)
		defer cancel()
		fmt.Println("Starting minikube")
		return runCommandContext(ctx,
			minikubeInitCmd,
			minikubeStartArgs,
			append(
				envVars,
				fmt.Sprintf("PATH=%s", path.Dir(vBoxManagePath)),
				fmt.Sprintf("$HOME=%s", os.Getenv("HOME")),
			),
		)
	}

	return nil
}

func runCommand(cmd string, args []string, extraEnvs []string) error {
	return runCommandContext(nil, cmd, args, extraEnvs)
}

func runCommandContext(ctx context.Context, cmd string, args []string, extraEnvs []string) error {
	var c *exec.Cmd
	if ctx == nil {
		c = exec.Command(cmd, args...)
	} else {
		c = exec.CommandContext(ctx, cmd, args...)
	}
	c.Env = append(c.Env, extraEnvs...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
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

		ctx, cancel := context.WithTimeout(context.Background(), kubectlApplyTimeout)
		defer cancel()
		err := runCommandContext(ctx,
			"kubectl",
			[]string{
				"--context",
				minikubeProfile,
				"--namespace",
				traefikNamespace,
				"apply",
				"--filename",
				manifest,
			},
			nil)
		if err != nil {
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
