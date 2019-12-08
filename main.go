package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	"io/ioutil"
	"k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

var (
	KUBECONFIG_PATH                   = "kubeconfig"
	KUBERNETES_SERVICE_PORT_HTTPS_ENV = "KUBERNETES_SERVICE_PORT_HTTPS"
	KUBERNETES_SERVICE_PORT_ENV       = "KUBERNETES_SERVICE_PORT"
	KUBERNETES_SERVICE_HOST_ENV       = "KUBERNETES_SERVICE_HOST"
	TARGETNAMESPACE_ENV               = "TARGETNAMESPACE"
	WORKLOADID_ENV                    = "WORKLOADID"
	PORT_ENV                          = "DEPLOYMENT_PORT"
	CONTAINERINDEX_ENV                = "CONTAINERINDEX"
	DEPLOY_SERVICE_ENV                = "DEPLOY_SERVICE"
	PLUGIN_REPO_ENV                   = "PLUGIN_REPO"
	PLUGIN_TAG_ENV                    = "PLUGIN_TAG"
	CICD_PROJECT_ID_ENV               = "CICD_PROJECT_ID"
	KUBECONFIG_TEMPLATE               = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: ${CA}
    server: ${SERVER}
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    user: jenkins
  name: jenkins@kubernetes
current-context: jenkins@kubernetes
kind: Config
preferences: {}
users:
- name: jenkins
  user:
    token: ${TOKEN}`
)

var client *kubernetes.Clientset

var deployConfig config

type config struct {
	Namespace     string
	Name          string
	DeployType    string
	Ports         []int32
	Index         int `json:"ContainerIndex"`
	Image         string
	PullSecret    string `json:"-"`
	DeployService bool
}

func main() {
	Init()

	pconfig := &deployConfig
	copyConfig := *pconfig
	copyConfig.Index = copyConfig.Index + 1
	bp, err := json.MarshalIndent(copyConfig, "", " ")
	if err == nil {
		fmt.Println("Deploy Workload Parameters: ")
		fmt.Println(string(bp))
	}

	switch deployConfig.DeployType {
	case "deployment":
		Deployment()
	default:
		fmt.Println("无法获取部署类型")
		os.Exit(2)
	}
	fmt.Println(fmt.Sprintf("命名空间[%s]部署工作负载[%s]成功", deployConfig.Namespace, deployConfig.Name))

	if deployConfig.DeployService {
		err := editService()
		if err == nil {
			fmt.Println(fmt.Sprintf("命名空间[%s]部署服务发现[%s]成功", deployConfig.Namespace, deployConfig.Name))
		} else {
			fmt.Println(err.Error())
		}
	}
}

func Deployment() {
	deploy := client.AppsV1().Deployments(deployConfig.Namespace)
	deployment, err := deploy.Get(deployConfig.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = deploy.Create(getNewDeployment())
		if err != nil && !apierrors.IsAlreadyExists(err) {
			fmt.Println(fmt.Sprintf("创建工作负载失败, err=[%v]", err))
			os.Exit(2)
		}
		return
	}
	deployment.Spec.Template.Spec.Containers[deployConfig.Index].Image = deployConfig.Image
	if len(deployConfig.Ports) != 0 {
		deployment.Spec.Template.Spec.Containers[deployConfig.Index].Ports = []corev1.ContainerPort{}
		for index, port := range deployConfig.Ports {
			deployment.Spec.Template.Spec.Containers[deployConfig.Index].Ports = append(deployment.Spec.Template.Spec.Containers[deployConfig.Index].Ports,
				corev1.ContainerPort{
					ContainerPort: port,
					Name:          fmt.Sprintf("%dtcp%02d", port, index+1),
				})
		}
	}
	_, err = deploy.Update(deployment)
	if err != nil {
		fmt.Println(fmt.Sprintf("更新工作负载失败, err=[%v]", err))
		os.Exit(2)
	}
}

func getNewDeployment() *v1.Deployment {
	replicas := int32(1)
	deploy := &v1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployConfig.Name,
			Namespace: deployConfig.Namespace,
			Labels: map[string]string{
				"workload.user.cattle.io/workloadselector": fmt.Sprintf("%s-%s-%s", deployConfig.DeployType, deployConfig.Namespace, deployConfig.Name),
				"cattle.io/creator":                        "norman",
			},
		},
		Spec: v1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"workload.user.cattle.io/workloadselector": fmt.Sprintf("%s-%s-%s", deployConfig.DeployType, deployConfig.Namespace, deployConfig.Name),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"workload.user.cattle.io/workloadselector": fmt.Sprintf("%s-%s-%s", deployConfig.DeployType, deployConfig.Namespace, deployConfig.Name),
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            deployConfig.Name,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Image:           deployConfig.Image,
						},
					},
					ImagePullSecrets: []corev1.LocalObjectReference{
						{Name: deployConfig.PullSecret},
					},
				},
			},
		},
	}
	if len(deployConfig.Ports) != 0 {
		for index, port := range deployConfig.Ports {
			deploy.Spec.Template.Spec.Containers[0].Ports = append(deploy.Spec.Template.Spec.Containers[0].Ports, corev1.ContainerPort{
				ContainerPort: port,
				Name:          fmt.Sprintf("%dtcp%02d", port, index+1),
			})
		}
	}
	return deploy
}

func editService() error {
	if len(deployConfig.Ports) == 0 {
		return nil
	}
	service := client.CoreV1().Services(deployConfig.Namespace)
	svc, err := service.Get(deployConfig.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		s := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deployConfig.Name,
				Namespace: deployConfig.Namespace,
				Labels: map[string]string{
					"cattle.io/creator": "norman",
				},
				Annotations: map[string]string{
					"field.cattle.io/targetWorkloadIds":       fmt.Sprintf(`["%s:%s:%s"]`, deployConfig.DeployType, deployConfig.Namespace, deployConfig.Name),
					"workload.cattle.io/targetWorkloadIdNoop": "true",
					"workload.cattle.io/workloadPortBased":    "true",
				},
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{
					"workload.user.cattle.io/workloadselector": fmt.Sprintf("%s-%s-%s", deployConfig.DeployType, deployConfig.Namespace, deployConfig.Name),
				},
				Type: corev1.ServiceTypeClusterIP,
			},
		}
		for index, port := range deployConfig.Ports {
			s.Spec.Ports = append(s.Spec.Ports,
				corev1.ServicePort{
					Name:     fmt.Sprintf("%dtcp%02d", port, index+1),
					Port:     port,
					Protocol: corev1.ProtocolTCP,
					TargetPort: intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: port,
					},
				})
		}
		_, err = service.Create(s)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			msg := fmt.Sprintf("创建服务发现失败, err=[%v]", err)
			fmt.Println(msg)
			return errors.New(msg)
		}
	} else {
		svc.Spec.Ports = []corev1.ServicePort{}
		for index, port := range deployConfig.Ports {
			svc.Spec.Ports = append(svc.Spec.Ports,
				corev1.ServicePort{
					Name:     fmt.Sprintf("%dtcp%02d", port, index+1),
					Port:     port,
					Protocol: corev1.ProtocolTCP,
					TargetPort: intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: port,
					},
				})
		}

		_, err = service.Update(svc)
		if err != nil {
			msg := fmt.Sprintf("更新服务发现失败, err=[%v]", err)
			fmt.Println(msg)
			return errors.New(msg)
		}
	}
	return nil
}

func GetKubeClient() *kubernetes.Clientset {
	clientConfig, err := clientcmd.BuildConfigFromFlags("", KUBECONFIG_PATH)
	if err != nil {
		fmt.Println(fmt.Sprintf("无法获取kubeconfig信息, err=[%v]", err))
		os.Exit(2)
	}
	clientset, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		fmt.Println(fmt.Sprintf("无法生成kubeclient, err=[%v]", err))
		os.Exit(2)
	}
	return clientset
}

func Init() {
	server := os.Getenv(KUBERNETES_SERVICE_HOST_ENV)
	if server == "" {
		fmt.Println("无法从环境变量(" + KUBERNETES_SERVICE_HOST_ENV + ")中获取kubernetes服务器地址")
		os.Exit(2)
	}
	kubehttpschema := "https"
	kubeport := os.Getenv(KUBERNETES_SERVICE_PORT_HTTPS_ENV)
	if kubeport == "" {
		kubeport = os.Getenv(KUBERNETES_SERVICE_PORT_ENV)
		if kubeport == "" {
			fmt.Println("无法从环境变量中获取kubernetes服务器端口")
			os.Exit(2)
		} else {
			kubehttpschema = "http"
		}
	}
	server = fmt.Sprintf("%s://%s:%s", kubehttpschema, server, kubeport)

	bca, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	//bca, err := ioutil.ReadFile("ca.crt")
	if err != nil {
		fmt.Println(fmt.Sprintf("无法获取CA信息, err=[%v]", err))
		os.Exit(2)
	}
	ca := base64.StdEncoding.EncodeToString(bca)

	//btoken, err := ioutil.ReadFile("token")
	btoken, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		fmt.Println(fmt.Sprintf("无法获取Token信息, err=[%v]", err))
		os.Exit(2)
	}
	token := string(btoken)
	kubeconfig := strings.ReplaceAll(KUBECONFIG_TEMPLATE, "${CA}", ca)
	kubeconfig = strings.ReplaceAll(kubeconfig, "${SERVER}", server)
	kubeconfig = strings.ReplaceAll(kubeconfig, "${TOKEN}", token)

	err = ioutil.WriteFile(KUBECONFIG_PATH, []byte(kubeconfig), 0666)
	if err != nil {
		fmt.Println(fmt.Sprintf("无法生成kube config文件, err=[%v]", err))
		os.Exit(2)
	}

	ns := os.Getenv(TARGETNAMESPACE_ENV)
	if ns == "" {
		fmt.Println("无法从环境变量(" + TARGETNAMESPACE_ENV + ")中获取命名空间信息")
		os.Exit(2)
	}
	deployConfig.Namespace = ns

	workloadId := os.Getenv(WORKLOADID_ENV)
	if workloadId == "" {
		fmt.Println("无法从环境变量(" + WORKLOADID_ENV + ")中获取工作负载信息")
		os.Exit(2)
	}
	ws := strings.Split(workloadId, ":")
	if len(ws) == 1 {
		deployConfig.Name = ws[0]
		deployConfig.DeployType = "deployment"
	} else if len(ws) == 3 {
		deployConfig.DeployType = ws[0]
		deployConfig.Name = ws[2]
	} else {
		fmt.Println("工作负载信息格式错误")
		os.Exit(2)
	}

	port := os.Getenv(PORT_ENV)
	if port != "" {
		pp := strings.Split(port, ",")
		for _, v := range pp {
			p, err := strconv.Atoi(v)
			if err == nil {
				deployConfig.Ports = append(deployConfig.Ports, int32(p))
			}
		}
	}

	index := os.Getenv(CONTAINERINDEX_ENV)
	i, _ := strconv.Atoi(index)
	if i > 0 {
		i = i - 1
	}
	deployConfig.Index = i

	ds := os.Getenv(DEPLOY_SERVICE_ENV)
	if ds == "true" {
		deployConfig.DeployService = true
	} else {
		deployConfig.DeployService = false
	}

	repo := os.Getenv(PLUGIN_REPO_ENV)
	if repo == "" {
		fmt.Println("无法从环境变量(" + PLUGIN_REPO_ENV + ")中获取镜像名称")
		os.Exit(2)
	}
	tag := os.Getenv(PLUGIN_TAG_ENV)
	if tag == "" {
		tag = "latest"
	}
	deployConfig.Image = fmt.Sprintf("%s:%s", repo, tag)

	client = GetKubeClient()

	project := os.Getenv(CICD_PROJECT_ID_ENV)
	pipelinens := fmt.Sprintf("%s-pipeline", project)
	secrets, err := client.CoreV1().Secrets(pipelinens).List(metav1.ListOptions{})
	if err != nil {
		fmt.Println(fmt.Sprintf("无法获取secrets信息, err=[%v]", err))
	} else {
		for _, v := range secrets.Items {
			if v.Type == corev1.SecretTypeDockerConfigJson {
				deployConfig.PullSecret = v.Name
				break
			}
		}
	}
}
