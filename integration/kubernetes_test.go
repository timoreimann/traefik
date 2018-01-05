package integration

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/client-go/pkg/util/yaml"

	"github.com/containous/traefik/provider/label"

	"golang.org/x/net/context"
	"k8s.io/client-go/pkg/util/intstr"

	"github.com/containous/traefik/integration/try"
	"github.com/containous/traefik/testhelpers"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"

	"github.com/go-check/check"
	checker "github.com/vdemeester/shakers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	minikubeProfile           = "traefik-integration-test"
	traefikNamespace          = "kube-system"
	kubernetesVersion         = "v1.8.0"
	minikubeStartupTimeout    = 90 * time.Second
	examplesRelativeDirectory = "../examples/k8s"
)

var (
	minikubeStartArgs = []string{
		"start",
		"--logtostderr",
		fmt.Sprintf("--kubernetes-version=%s", kubernetesVersion),
		"--extra-config=apiserver.Authorization.Mode=RBAC",
		"--keep-context",
		"--disk-size=5g",
	}

	minikubeEnvVars = []string{
		"MINIKUBE_WANTUPDATENOTIFICATION=false",
		"MINIKUBE_WANTREPORTERRORPROMPT=false",
		fmt.Sprintf("MINIKUBE_PROFILE=%s", minikubeProfile),
	}
)

type KubernetesSuite struct {
	BaseSuite
	client   *kubernetes.Clientset
	nodeHost string
	master   *url.URL
}

type kubeManifests []string

func (km *kubeManifests) Apply(names ...string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current working directory: %s", err)
	}

	for _, name := range names {
		manifest := filepath.Join(cwd, examplesRelativeDirectory, name)
		cmd := exec.Command(
			"kubectl",
			"--context",
			minikubeProfile,
			"--namespace",
			traefikNamespace,
			"apply",
			"--filename",
			manifest)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to apply manifest %s: %s", manifest, err)
		}

		*km = append(*km, manifest)
	}

	return nil
}

func (km *kubeManifests) DeleteApplied() error {
	for _, manifest := range *km {
		cmd := exec.Command(
			"kubectl",
			"--context",
			minikubeProfile,
			"--namespace",
			traefikNamespace,
			"delete",
			"--grace-period=5",
			"--filename",
			manifest)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to delete manifest %s: %s", manifest, err)
		}
	}

	return nil
}

func (s *KubernetesSuite) SetUpSuite(c *check.C) {
	err := checkRequirements()
	c.Assert(err, checker.IsNil, check.Commentf("requirements failed: %s", err))

	// TODO: Move to function.
	minikubeInitCmd := "minikube"
	// Configure minikube if we run on the CI system.
	if os.Getenv("CI") != "" {
		// Bootstrap Kubernetes natively on the host system.
		minikubeStartArgs = append(minikubeStartArgs, "--vm-driver=none")
		// Native Kubernetes requires root privileges.
		minikubeInitCmd = "sudo"
		minikubeStartArgs = append([]string{"--preserve-env", "minikube"}, minikubeStartArgs...)
		// Make sure root-owned files are moved to the proper location.
		minikubeEnvVars = append(minikubeEnvVars, "CHANGE_MINIKUBE_NONE_USER=true")
	}

	// TODO: Move to function.
	cmd := exec.Command("minikube", "status")
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), minikubeEnvVars...)
	if err := cmd.Run(); err != nil {
		_, ok := err.(*exec.ExitError)
		c.Assert(ok, checker.True, check.Commentf("\"minikube status\" failed: %s", err))

		// Start minikube.
		// TODO: stop after usage
		ctx, cancel := context.WithTimeout(context.Background(), minikubeStartupTimeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, minikubeInitCmd, minikubeStartArgs...)
		cmd.Env = append(os.Environ(), minikubeEnvVars...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		c.Assert(err, checker.IsNil)
	}

	// TODO: Move to function.
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{
			CurrentContext: minikubeProfile,
			Context: clientcmdapi.Context{
				Namespace: traefikNamespace,
			},
		},
	).ClientConfig()
	c.Assert(err, checker.IsNil)

	client, err := kubernetes.NewForConfig(config)
	c.Assert(err, checker.IsNil)

	master, err := url.Parse(config.Host)
	c.Assert(err, checker.IsNil)

	s.nodeHost = master.Hostname()
	s.master = master
	s.client = client

	// Wait for cluster to become ready.
	err = try.Do(1*time.Minute, func() error {
		_, err := s.client.ServerVersion()
		return err
	})
	c.Assert(err, checker.IsNil)
}

func (s *KubernetesSuite) TestManifestExamples(c *check.C) {
	manifests := kubeManifests{}
	defer func() {
		err := manifests.DeleteApplied()
		c.Assert(err, checker.IsNil)
	}()

	// Validate Traefik is reachable.
	err := manifests.Apply(
		"traefik-rbac.yaml",
		"traefik-deployment.yaml",
	)
	c.Assert(err, checker.IsNil)

	// Get the service NodePort.
	svcName, err := getNameFromManifest("traefik-deployment.yaml", "Service")
	c.Assert(err, checker.IsNil, check.Commentf("failed to get service: %s", err))

	svc, err := s.client.Services(traefikNamespace).Get(svcName)
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
	c.Assert(err, checker.IsNil, check.Commentf("traefik UI access (req: %s)", *req))

	// Validate third-party service is routable through Traefik.
	err = manifests.Apply(
		"cheese-deployments.yaml",
		"cheese-services.yaml",
		"cheese-ingress.yaml",
	)
	c.Assert(err, checker.IsNil)

	req = testhelpers.MustNewRequest(http.MethodGet, baseTraefikURL, nil)
	for _, svc := range []string{"stilton", "cheddar", "wensleydale"} {
		host := svc + ".minikube"
		req.Host = host
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

	// pods, err := client.CoreV1().Pods(api.NamespaceDefault).List(v1.ListOptions{})
	// c.Assert(err, checker.IsNil)

	// fmt.Printf("Got %d pod(s) in default namespace\n", len(pods.Items))
	// for i, pod := range pods.Items {
	// 	fmt.Printf("#%d: %s\n", i+1, pod.ObjectMeta.Name)
	// }

	_, err = s.client.ExtensionsV1beta1().Ingresses(api.NamespaceDefault).Create(
		&v1beta1.Ingress{
			ObjectMeta: v1.ObjectMeta{
				Name: "whoami",
				Annotations: map[string]string{
					label.TraefikFrontendRuleType: "Path",
				},
			},
			Spec: v1beta1.IngressSpec{
				Rules: []v1beta1.IngressRule{
					v1beta1.IngressRule{
						IngressRuleValue: v1beta1.IngressRuleValue{
							HTTP: &v1beta1.HTTPIngressRuleValue{
								Paths: []v1beta1.HTTPIngressPath{
									v1beta1.HTTPIngressPath{
										Path: "/service",
										Backend: v1beta1.IngressBackend{
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

	_, err = s.client.CoreV1().Services(api.NamespaceDefault).Create(
		&v1.Service{
			ObjectMeta: v1.ObjectMeta{
				Name: "whoami",
			},
			Spec: v1.ServiceSpec{
				Ports: []v1.ServicePort{
					v1.ServicePort{
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

	_, err = s.client.Pods(api.NamespaceDefault).Create(
		&v1.Pod{
			ObjectMeta: v1.ObjectMeta{
				Name: "whoami",
				Labels: map[string]string{
					"app": "whoami",
				},
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					v1.Container{
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

func getNameFromManifest(name, kind string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current working directory: %s", err)
	}

	manifest := filepath.Join(cwd, examplesRelativeDirectory, name)

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
