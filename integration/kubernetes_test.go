package integration

import (
	"bytes"
	"encoding/json"
	"errors"
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

	"github.com/Masterminds/semver"
	"github.com/containous/traefik/integration/try"
	"github.com/containous/traefik/testhelpers"
	"github.com/go-check/check"
	checker "github.com/vdemeester/shakers"
	"golang.org/x/net/context"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	traefikNamespace          = "traefik"
	kubernetesVersion         = "v1.9.0"
	minVersionMinikube        = "v0.25.2"
	minVersionKubectl         = "v1.7.0"
	minikubeStartupTimeout    = 2 * time.Minute
	minikubeDeleteTimeout     = 30 * time.Second
	kubectlApplyTimeout       = 60 * time.Second
	examplesRelativeDirectory = "examples/k8s"
	envVarSkipVMCleanup       = "K8S_SKIP_CLEANUP"
	envVarMinikubeProfile     = "K8S_MINIKUBE_PROFILE"
)

var (
	versionConstraintMinikube *semver.Constraints
	versionConstraintKubectl  *semver.Constraints
	minikubeProfile           string
	kubeconfig                string
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

type versionMismatch struct {
	component string
	got       string
	min       string
}

type errorRequirementMissing string

func (e errorRequirementMissing) Error() string {
	return fmt.Sprintf("component %q is missing", e)
}

type errorRequirementVersionMismatch struct {
	component string
	got       string
	min       string
}

func (e errorRequirementVersionMismatch) Error() string {
	return fmt.Sprintf("component %q does not satisfy minimum version requirement: %s < %s", e.component, e.got, e.min)
}

type KubeConnection struct {
	client           *kubernetes.Clientset
	dynamic          dynamic.ClientPool
	apiResourcesByGK map[schema.GroupKind]metav1.APIResource
	nodeHost         string
	master           *url.URL
}

type KubernetesSuite struct {
	BaseSuite
	KubeConnection
	cleanupAfterCompletion bool
}

func init() {
	constraint := fmt.Sprintf(">= %s", minVersionMinikube)
	var err error
	versionConstraintMinikube, err = semver.NewConstraint(constraint)
	if err != nil {
		panic(fmt.Sprintf("failed to parse minikube version constraint %q: %s", constraint, err))
	}

	constraint = fmt.Sprintf(">= %s", minVersionKubectl)
	versionConstraintKubectl, err = semver.NewConstraint(constraint)
	if err != nil {
		panic(fmt.Sprintf("failed to parse kubectl version constraint %q: %s", constraint, err))
	}
}

func (s *KubernetesSuite) SetUpSuite(c *check.C) {
	errs := checkRequirements()
	c.Assert(errs, checker.IsNil, check.Commentf("requirements failed: %s", errs))

	onCI := os.Getenv("CI") != ""
	err := setMinikubeParams(onCI)
	c.Assert(err, checker.IsNil, check.Commentf("failed to set minikube parameters: %s", err))

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

	// RBAC rules sometimes fail with "no matches for rbac.authorization.k8s.io/".
	// Retrying help, so this looks like a bootstrapping issue.
	// err := try.Do(10*time.Second, func() error {
	// 	return s.Apply("", "traefik-rbac.yaml")
	// })
	err := s.Apply("", "traefik-rbac.yaml")
	c.Assert(err, checker.IsNil)

	err = s.Apply(traefikNamespace, patchedDeployment)
	c.Assert(err, checker.IsNil)

	defer func() {
		if c.Failed() {
			fmt.Println("Traefik pod description:")
			runKubectl("describe", "pod", "-l", "k8s-app=traefik-ingress-lb")

			fmt.Println("Traefik pod logs:")
			runKubectl("logs", "deploy/traefik-ingress-controller")

			if os.Getenv("CI") != "" {
				fmt.Println("Disk size:")
				runCommand("df", []string{"-h"}, commandParams{})
			}
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
	err = s.Apply(traefikNamespace,
		"ui.yaml",
	)
	c.Assert(err, checker.IsNil)

	req := testhelpers.MustNewRequest(http.MethodGet, baseTraefikURL+"dashboard/", nil)
	req.Host = "traefik-ui.minikube"
	err = try.Request(req, 3*time.Second, try.StatusCodeIs(http.StatusOK))
	c.Assert(err, checker.IsNil, check.Commentf("traefik UI access (req: %s host: %s headers: %s)", *req, req.Host, req.Header))

	// Validate third-party service is routable through Traefik.
	err = s.Apply(metav1.NamespaceDefault,
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

func checkRequirements() []error {
	var reqErrors []error
	if err := checkRequirement(
		"minikube version",
		func(output []byte) (*semver.Version, error) {
			fields := strings.Fields(string(output))
			versionField := fields[len(fields)-1]
			v, err := semver.NewVersion(versionField)
			if err != nil {
				return nil, fmt.Errorf("failed to parse version %q: %s", versionField, err)
			}
			return v, nil
		},
		versionConstraintMinikube,
		minVersionMinikube,
	); err != nil {
		reqErrors = append(reqErrors, err)
	}

	if err := checkRequirement(
		"kubectl version --client=true --output=json",
		func(output []byte) (*semver.Version, error) {
			var kubectlOutput struct {
				ClientVersion struct {
					GitVersion string `json:"gitVersion"`
				} `json:"clientVersion"`
			}
			if err := json.Unmarshal(output, &kubectlOutput); err != nil {
				return nil, fmt.Errorf("failed to unmarshal output: %s", err)
			}
			gitVersion := kubectlOutput.ClientVersion.GitVersion
			if gitVersion == "" {
				return nil, errors.New("git version from kubectl output is empty")
			}
			v, err := semver.NewVersion(gitVersion)
			if err != nil {
				return nil, fmt.Errorf("failed to parse version %q: %s", gitVersion, err)
			}
			return v, nil
		},
		versionConstraintKubectl,
		minVersionKubectl,
	); err != nil {
		reqErrors = append(reqErrors, err)
	}

	return reqErrors
}

func checkRequirement(commandLine string, parser func(output []byte) (*semver.Version, error), versionConstraint *semver.Constraints, minVersion string) error {
	cmdComponents := strings.Fields(commandLine)
	switch len(cmdComponents) {
	case 0:
		return errors.New("command line not specified")
	case 1:
		return errors.New("version check arguments missing")
	}

	cmd := cmdComponents[0]
	cmdPath, err := exec.LookPath(cmd)
	if err != nil {
		return errorRequirementMissing(cmd)
	}

	version, err := parseBinaryVersion(cmdPath, cmdComponents[1:], parser)
	if err != nil {
		return fmt.Errorf("failed to parse binary version of %q: %s", cmdPath, err)
	}
	if chk := versionConstraint.Check(version); !chk {
		return errorRequirementVersionMismatch{
			component: cmd,
			got:       version.String(),
			min:       minVersion,
		}
	}

	return nil
}

func parseBinaryVersion(cmd string, args []string, parser func([]byte) (*semver.Version, error)) (*semver.Version, error) {
	var buf bytes.Buffer
	if err := runCommand(cmd, args, commandParams{stdout: &buf}); err != nil {
		return nil, fmt.Errorf("failed to run command: %s", err)
	}
	if buf.Len() == 0 {
		return nil, errors.New("output of command is empty")
	}
	v, err := parser(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("failed to parse command output %q: %s", buf.String(), err)
	}
	return v, nil
}

func setMinikubeParams(onCI bool) error {
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
	fmt.Printf("Using minikube home %q\n", minikubeHome)
	minikubeEnvVars = append(minikubeEnvVars, fmt.Sprintf("MINIKUBE_HOME=%s", minikubeHome))

	kubeconfig = path.Join("/", os.Getenv("HOME"), ".kube", "config")
	if err := os.MkdirAll(filepath.Dir(kubeconfig), os.ModePerm); err != nil {
		return fmt.Errorf("failed to create kubeconfig directory %q: %s", filepath.Dir(kubeconfig), err)
	}
	if _, err := os.OpenFile(kubeconfig, os.O_RDWR|os.O_CREATE, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create empty kubeconfig file %q: %s", kubeconfig, err)
	}
	fmt.Printf("Using kubeconfig %q\n", kubeconfig)
	minikubeEnvVars = append(minikubeEnvVars, fmt.Sprintf("KUBECONFIG=%s", kubeconfig))

	if !onCI {
		vBoxManagePath, err := exec.LookPath("VBoxManage")
		if err != nil {
			return fmt.Errorf("cannot find VBoxManage path: %s", err)
		}
		minikubeEnvVars = append(minikubeEnvVars, fmt.Sprintf("PATH=%s", path.Dir(vBoxManagePath)))
	}

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

	dyn := dynamic.NewDynamicClientPool(config)

	disc := discovery.NewDiscoveryClient(client.RESTClient())
	resLists, err := disc.ServerResources()
	if err != nil {
		return nil, fmt.Errorf("failed to list server resources: %s", err)
	}
	resourcesByGK := map[schema.GroupKind]metav1.APIResource{}
	for _, resList := range resLists {
		fmt.Printf("Found GroupVersion: %s\n", resList.GroupVersion)
		for _, res := range resList.APIResources {
			if strings.ContainsRune(res.Name, '/') {
				continue
			}
			gv, err := schema.ParseGroupVersion(resList.GroupVersion)
			if err != nil {
				panic(fmt.Sprintf("failed to parse group version %q: %s", resList.GroupVersion, err))
			}
			if res.Group == "" {
				res.Group = gv.Group
			}
			if res.Version == "" {
				res.Version = gv.Version
			}
			fmt.Printf("Found APIResource: %#+v\n", res)
			resourcesByGK[schema.GroupKind{Group: gv.Group, Kind: res.Kind}] = res
		}
	}

	master, err := url.Parse(config.Host)
	if err != nil {
		return nil, fmt.Errorf("failed to parse host URL %q: %s", config.Host, err)
	}

	return &KubeConnection{
		nodeHost:         master.Hostname(),
		master:           master,
		client:           client,
		dynamic:          dyn,
		apiResourcesByGK: resourcesByGK,
	}, nil
}

func (s *KubernetesSuite) Apply(namespace string, names ...string) error {
	for _, name := range names {
		manifest := name
		if !path.IsAbs(manifest) {
			manifest = createAbsoluteManifestPath(manifest)
		}

		file, err := os.Open(manifest)
		if err != nil {
			return err
		}
		defer file.Close()

		dec := yaml.NewYAMLToJSONDecoder(file)
		for {
			var obj unstructured.Unstructured
			err := dec.Decode(&obj)
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}

			gvk := obj.GroupVersionKind()
			apiRes, ok := s.apiResourcesByGK[gvk.GroupKind()]
			if !ok {
				return fmt.Errorf("failed to find resource by group kind %s", gvk.GroupKind())
			}

			dynCl, err := s.dynamic.ClientForGroupVersionKind(gvk)
			if err != nil {
				return fmt.Errorf("failed to create dynamic client for GVR %q: %s", gvk, err)
			}

			ns := obj.GetNamespace()
			if ns == "" {
				ns = namespace
			}
			fmt.Printf("APIResource is: %#+v\n", apiRes)
			res := dynCl.Resource(&apiRes, ns)

			if _, err := res.Create(&obj); err != nil {
				return fmt.Errorf("failed to apply manifest %s: %s", manifest, err)
			}
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
