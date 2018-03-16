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
	"github.com/containous/traefik/provider/label"
	"github.com/containous/traefik/testhelpers"
	"github.com/go-check/check"
	checker "github.com/vdemeester/shakers"
	"golang.org/x/net/context"
	corev1 "k8s.io/api/core/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	minikubeProfile           = "traefik-integration-test"
	traefikNamespace          = "traefik"
	kubernetesVersion         = "v1.8.0"
	minikubeStartupTimeout    = 90 * time.Second
	minikubeStopTimeout       = 30 * time.Second
	examplesRelativeDirectory = "examples/k8s"
	envVarFlagSkipVMCleanup   = "K8S_SKIP_VM_CLEANUP"
)

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
		fmt.Sprintf("MINIKUBE_PROFILE=%s", minikubeProfile),
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

type kubeManifests []string

func (km *kubeManifests) Apply(names ...string) error {
	for _, name := range names {
		manifest := name
		if !path.IsAbs(manifest) {
			manifest = createAbsoluteManifestPath(manifest)
		}

		err := runCommand("kubectl",
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

		*km = append(*km, manifest)
	}

	return nil
}

// TODO: Probably remove since we want to delete the entire namespace after
// each test.
func (km *kubeManifests) DeleteApplied() error {
	if len(*km) > 0 {
		fmt.Printf("Deleting %d manifest(s)\n", len(*km))
	}
	for _, manifest := range *km {
		err := runCommand("kubectl",
			[]string{
				"--context",
				minikubeProfile,
				"--namespace",
				traefikNamespace,
				"delete",
				"--grace-period=5",
				"--filename",
				manifest,
			},
			nil)
		if err != nil {
			return fmt.Errorf("failed to delete manifest %s: %s", manifest, err)
		}
	}

	// TODO: Can tests finish before all objects are truly deleted? Do we need to poll for assured deletion?

	return nil
}

func (s *KubernetesSuite) SetUpSuite(c *check.C) {
	err := checkRequirements()
	c.Assert(err, checker.IsNil, check.Commentf("requirements failed: %s", err))

	onCI := os.Getenv("CI") != ""

	err = startMinikube(onCI)
	c.Assert(err, checker.IsNil, check.Commentf("failed to start minikube: %s", err))

	skipStop := os.Getenv(envVarFlagSkipVMCleanup) != ""
	if !skipStop && !onCI {
		s.stopAfterCompletion = true
	}

	if !onCI {
		// Transfer current Traefik image into minikube. This is not necessary
		// on the CI where we use the host-based none driver.
		fmt.Println("Transferring current Traefik image into minikube")
		err := runCommand("bash",
			[]string{
				"-c",
				fmt.Sprintf("docker save traefik:kube-test | (eval $(minikube docker-env --alsologtostderr -p %s) && docker load)", minikubeProfile),
			},
			minikubeEnvVars)
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
	err := s.client.CoreV1().Namespaces().Delete(traefikNamespace, &metav1.DeleteOptions{
		PropagationPolicy: &policy,
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
	manifests := kubeManifests{}
	// defer func() {
	// 	err := manifests.DeleteApplied()
	// 	c.Assert(err, checker.IsNil)
	// }()

	// Use Deployment manifest referencing current traefik binary.
	patchedDeployment := createAbsolutePath(fmt.Sprintf("integration/resources/%s.test.yaml", workloadManifest))

	// Validate Traefik is reachable.
	err := manifests.Apply(
		"traefik-rbac.yaml",
		patchedDeployment,
	)
	c.Assert(err, checker.IsNil)
	defer func() {
		if c.Failed() {
			fmt.Println("Traefik pod description:")
			runCommand("kubectl",
				[]string{
					"kubectl",
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
	err = manifests.Apply(
		"ui.yaml",
	)
	c.Assert(err, checker.IsNil)

	req := testhelpers.MustNewRequest(http.MethodGet, baseTraefikURL+"dashboard/", nil)
	req.Host = "traefik-ui.minikube"
	err = try.Request(req, 3*time.Second, try.StatusCodeIs(http.StatusOK))
	c.Assert(err, checker.IsNil, check.Commentf("traefik UI access (req: %s host: %s headers: %s)", *req, req.Host, req.Header))

	// Validate third-party service is routable through Traefik.
	err = manifests.Apply(
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

func (s *KubernetesSuite) TestBasic(c *check.C) {
	// Start Traefik.
	file := s.adaptFile(c, "fixtures/kubernetes/simple.toml", struct {
		MasterURL string
	}{s.master.String()})
	defer os.Remove(file)
	cmd, display := s.traefikCmd(withConfigFile(file))
	defer display(c)
	err := cmd.Start()
	c.Assert(err, checker.IsNil)
	defer cmd.Process.Kill()

	// pods, err := client.CoreV1().Pods(metav1.NamespaceDefault).List(corev1.ListOptions{})
	// c.Assert(err, checker.IsNil)

	// fmt.Printf("Got %d pod(s) in default namespace\n", len(pods.Items))
	// for i, pod := range pods.Items {
	// 	fmt.Printf("#%d: %s\n", i+1, pod.ObjectMeta.Name)
	// }

	_, err = s.client.ExtensionsV1beta1().Ingresses(metav1.NamespaceDefault).Create(
		&extensionsv1beta1.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Name: "whoami",
				Annotations: map[string]string{
					label.TraefikFrontendRuleType: "Path",
				},
			},
			Spec: extensionsv1beta1.IngressSpec{
				Rules: []extensionsv1beta1.IngressRule{
					extensionsv1beta1.IngressRule{
						IngressRuleValue: extensionsv1beta1.IngressRuleValue{
							HTTP: &extensionsv1beta1.HTTPIngressRuleValue{
								Paths: []extensionsv1beta1.HTTPIngressPath{
									extensionsv1beta1.HTTPIngressPath{
										Path: "/service",
										Backend: extensionsv1beta1.IngressBackend{
											ServiceName: "whoami",
											ServicePort: intstr.FromInt(8080),
										},
									},
								},
							},
						},
					},
				},
			},
		},
	)
	c.Assert(err, checker.IsNil)

	_, err = s.client.CoreV1().Services(metav1.NamespaceDefault).Create(
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: "whoami",
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{
					corev1.ServicePort{
						Port:       int32(8080),
						TargetPort: intstr.FromInt(80),
					},
				},
				Selector: map[string]string{
					"app": "whoami",
				},
			},
		},
	)
	c.Assert(err, checker.IsNil)

	_, err = s.client.CoreV1().Pods(metav1.NamespaceDefault).Create(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "whoami",
				Labels: map[string]string{
					"app": "whoami",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					corev1.Container{
						Image: "emilevauge/whoami",
						Name:  "whoami",
					},
				},
			},
		},
	)
	c.Assert(err, checker.IsNil)

	// Query application via Traefik.
	err = try.GetRequest("http://127.0.0.1:8000/service", 45*time.Second, try.StatusCodeIs(http.StatusOK))
	c.Assert(err, checker.IsNil)
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

func startMinikube(onCI bool) error {
	// Check if minikube is already running.
	cmd := exec.Command("minikube", "status")
	// TODO: Use dynamically created profile name to avoid conflicts in CI
	// situations unless enforced via custom env var.
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
		return runCommandContext(ctx, minikubeInitCmd, minikubeStartArgs, envVars)
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
