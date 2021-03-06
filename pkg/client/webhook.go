package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"k8s.io/api/admission/v1beta1"
	restclient "k8s.io/client-go/rest"
)

const (
	patchedLabel string = "lazykube/patched"
	configName   string = "lazykube-config"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()

	defaulter = runtime.ObjectDefaulter(runtimeScheme)
)

var ignoredNamespaces = []string{
	metav1.NamespaceSystem,
	metav1.NamespacePublic,
}

const (
	admissionWebhookAnnotationInjectKey = "lazykube.myway5.com/inject"
	admissionWebhookAnnotationStatusKey = "lazykube.myway5.com/status"
)

// WebhookServer 提供接口调用
type WebhookServer struct {
	server *http.Server
}

// NewWebhookServer 新建 webhook server
func NewWebhookServer(params *WhSvrParameters) (*WebhookServer, error) {

	pair, err := tls.LoadX509KeyPair(params.CertFile, params.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("Failed to load key pair: %v", err)
	}

	ws := &WebhookServer{
		server: &http.Server{
			Addr:      fmt.Sprintf(":%v", params.Port),
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}},
		},
	}

	return ws, nil
}

// WhSvrParameters webhook server parameters
type WhSvrParameters struct {
	Port     int    // webhook server port
	CertFile string // path to the x509 certificate for https
	KeyFile  string // path to the x509 private key matching `CertFile`
}

type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = admissionregistrationv1beta1.AddToScheme(runtimeScheme)
	// defaulting with webhooks
	_ = v1.AddToScheme(runtimeScheme)

	// default replace strategy, avoid lack of configmap
	globalConfig.RegisterReplaceStrategy("quay.io", PrefixReplace, "quay.azk8s.cn")
	globalConfig.RegisterReplaceStrategy("gcr.io", PrefixReplace, "gcr.azk8s.cn")
	globalConfig.RegisterReplaceStrategy("k8s.gcr.io", PrefixReplace, "gcr.azk8s.cn/google-containers")
	globalConfig.RegisterReplaceStrategy("docker.io", PrefixReplace, "dockerhub.azk8s.cn")
	globalConfig.RegisterReplaceStrategy("default", DefaultReplace, "dockerhub.azk8s.cn")
}

// Check whether the pod need to be mutated
func mutateRequired(pod *corev1.Pod) bool {
	for key := range pod.Labels {
		if key == patchedLabel {
			return false
		}
	}

	return true
}

func patchContainers(pod *corev1.Pod) []patchOperation {
	var patch = make([]patchOperation, 0)

	// replace initContainers
	for i := range pod.Spec.InitContainers {
		pod.Spec.InitContainers[i].Image = globalConfig.Replace(pod.Spec.InitContainers[i].Image)
	}

	if pod.Spec.InitContainers != nil && len(pod.Spec.InitContainers) != 0 {
		var initPatch patchOperation

		initPatch.Op = "replace"
		initPatch.Path = "/spec/initContainers"
		initPatch.Value = pod.Spec.InitContainers

		patch = append(patch, initPatch)
	}

	// replace containers
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].Image = globalConfig.Replace(pod.Spec.Containers[i].Image)
	}

	var containerPatch patchOperation
	containerPatch.Op = "replace"
	containerPatch.Path = "/spec/containers"
	containerPatch.Value = pod.Spec.Containers

	patch = append(patch, containerPatch)

	return patch
}

func patchLabels(pod *corev1.Pod) patchOperation {
	var patch patchOperation
	patch.Op = "add"
	patch.Path = "/metadata/labels"

	label := make(map[string]string, 0)
	label[patchedLabel] = "true"

	patch.Value = label

	return patch
}

func createPatch(pod *corev1.Pod) ([]byte, error) {
	var patches []patchOperation

	containersPatches := patchContainers(pod)
	patches = append(patches, containersPatches...)

	// labelsPatch := patchLabels(pod)
	// patches = append(patches, labelsPatch)

	return json.Marshal(patches)
}

func (whsrv *WebhookServer) mutate(ar *v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	req := ar.Request
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		log.Errorf("Could not unmarshal raw object: %v\n", err)

		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	log.Infof("AdmissionReview for Kind=%v, Namespace=%v (%v) UID=%v patchOperation=%v UserInfo=%v\n",
		req.Kind, req.Namespace, req.Name, req.UID, req.Operation, req.UserInfo)

	// determine whether to perform mutation
	if !mutateRequired(&pod) {
		log.Infof("Skipping mutation for %s/%s due to policy check \n", pod.Namespace, pod.Name)
		return &v1beta1.AdmissionResponse{
			Allowed: true,
		}
	}

	patchBytes, err := createPatch(&pod)
	if err != nil {
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	log.Infof("AdmissionResponse: patch=%v\n", string(patchBytes))
	return &v1beta1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *v1beta1.PatchType {
			pt := v1beta1.PatchTypeJSONPatch
			return &pt
		}(),
	}
}

func (whsrv *WebhookServer) serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}

	if len(body) == 0 {
		log.Error("empty body")
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		log.Errorf("Content-Type=%s, expect application/json", contentType)
		http.Error(w, "invalid Content-Type, expect `application/json`", http.StatusUnsupportedMediaType)
		return
	}

	var admissionResponse *v1beta1.AdmissionResponse
	ar := v1beta1.AdmissionReview{}
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		log.Errorf("Can't decode body: %v", err)
		admissionResponse = &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	} else {
		admissionResponse = whsrv.mutate(&ar)
	}

	admissionReview := v1beta1.AdmissionReview{}
	if admissionResponse != nil {
		admissionReview.Response = admissionResponse
		if ar.Request != nil {
			admissionReview.Response.UID = ar.Request.UID
		}
	}

	resp, err := json.Marshal(admissionReview)
	if err != nil {
		log.Errorf("Can't encode response: %v", err)
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}
	log.Infoln("Ready to write response ...")
	if _, err := w.Write(resp); err != nil {
		log.Errorf("Can't write response: %v", err)
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}

	log.Infoln("mutate finished")
}

// Start 启动服务
func (whsrv *WebhookServer) Start() error {
	k8sCli, err := getKubeClient()
	if err != nil {
		return err
	}

	var namespace string
	if namespace = os.Getenv("NAMESPACE"); namespace == "" {
		return fmt.Errorf("NAMESPACE environment variable doesn't exist")
	}

	// 同步configmap
	cmWatcher := NewConfigMapWatcher(k8sCli, namespace, configName, globalConfig)
	if err := cmWatcher.SyncConfig(); err != nil {
		return err
	}
	go cmWatcher.Run(context.TODO())

	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", whsrv.serve)
	whsrv.server.Handler = mux

	if err := whsrv.server.ListenAndServeTLS("", ""); err != nil {
		return fmt.Errorf("Failed to listen and serve webhook server: %v", err)
	}

	return nil
}

// Shutdown 终止
func (whsrv *WebhookServer) Shutdown() {
	log.Info("Got OS shutdown signal, shutting down webhook server gracefully...")
	whsrv.server.Shutdown(context.Background())
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}

	return os.Getenv("USERPROFILE")
}

func getKubeConfig() (*restclient.Config, error) {
	var kubeconfig string

	// TODO: 目前临时从home下加载配置
	home := homeDir()
	kubeconfig = filepath.Join(home, ".kube", "config")

	var config *restclient.Config

	// 判断有没有文件
	_, err := os.Stat(kubeconfig)
	if err == nil {
		// 使用kubeconfig中当前的上下文环境
		log.Info("use local kube config file")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
	} else {
		// 使用 incluster 配置
		log.Info("use in cluster config")
		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
	}

	return config, nil
}

// getKubeClient 获取k8s的客户端
func getKubeClient() (*kubernetes.Clientset, error) {

	config, err := getKubeConfig()
	if err != nil {
		return nil, fmt.Errorf("get kubeconfig error: %v", err)
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, err
}
