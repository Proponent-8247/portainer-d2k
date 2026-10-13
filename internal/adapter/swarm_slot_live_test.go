//go:build livek8s

package adapter

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/portainer/d2k/internal/types"
)

func liveSlotTestAdapter(t *testing.T, replicas int32) (*KubernetesDockerAdapter, appsv1.Deployment) {
	t.Helper()
	if os.Getenv("D2K_LIVE_K8S") != "1" {
		t.Skip("set D2K_LIVE_K8S=1 with a disposable kubeconfig to run live-controller acceptance")
	}
	kubeconfig:=os.Getenv("KUBECONFIG")
	if kubeconfig=="" {t.Fatal("KUBECONFIG is required")}
	cfg,err:=clientcmd.BuildConfigFromFlags("",kubeconfig)
	if err!=nil {t.Fatalf("load kubeconfig: %v",err)}
	client,err:=kubernetes.NewForConfig(cfg)
	if err!=nil {t.Fatalf("Kubernetes client: %v",err)}
	ns:=fmt.Sprintf("d2k-prr-%d",time.Now().UnixNano())
	ctx,cancel:=context.WithTimeout(context.Background(),2*time.Minute)
	defer cancel()
	if _,err:=client.CoreV1().Namespaces().Create(ctx,&corev1.Namespace{ObjectMeta:metav1.ObjectMeta{Name:ns}},metav1.CreateOptions{});err!=nil {
		t.Fatalf("create namespace: %v",err)
	}
	t.Cleanup(func(){
		ctx,cancel:=context.WithTimeout(context.Background(),30*time.Second)
		defer cancel()
		_ = client.CoreV1().Namespaces().Delete(ctx,ns,metav1.DeleteOptions{})
	})
	a:=&KubernetesDockerAdapter{client:client,namespace:ns,logger:zap.NewNop().Sugar()}
	labels:=map[string]string{
		"app":"web",
		types.LabelManagedBy:types.LabelManagedByValue,
		types.LabelSwarmManagedBy:types.LabelSwarmManagedByValue,
		types.LabelSwarmService:"web",
	}
	surge:=intstr.FromInt(1)
	unavailable:=intstr.FromInt(0)
	dep:=&appsv1.Deployment{
		ObjectMeta:metav1.ObjectMeta{Name:"web",Namespace:ns,Labels:labels},
		Spec:appsv1.DeploymentSpec{
			Replicas:&replicas,
			Selector:&metav1.LabelSelector{MatchLabels:map[string]string{"app":"web"}},
			Strategy:appsv1.DeploymentStrategy{
				Type:appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate:&appsv1.RollingUpdateDeployment{MaxSurge:&surge,MaxUnavailable:&unavailable},
			},
			Template:corev1.PodTemplateSpec{
				ObjectMeta:metav1.ObjectMeta{Labels:labels},
				Spec:corev1.PodSpec{
					Containers:[]corev1.Container{{
						Name:"web",
						Image:"registry.k8s.io/pause:3.10",
						ImagePullPolicy:corev1.PullIfNotPresent,
					}},
					ReadinessGates:[]corev1.PodReadinessGate{{ConditionType:corev1.PodConditionType(types.HealthReadinessGate)}},
				},
			},
		},
	}
	created,err:=client.AppsV1().Deployments(ns).Create(ctx,dep,metav1.CreateOptions{})
	if err!=nil {t.Fatalf("create Deployment: %v",err)}
	return a,*created
}

func livePods(ctx context.Context,a *KubernetesDockerAdapter)([]corev1.Pod,error){
	pods,err:=a.client.CoreV1().Pods(a.namespace).List(ctx,metav1.ListOptions{
		LabelSelector:"app=web,"+types.LabelSwarmManagedBy+"="+types.LabelSwarmManagedByValue,
	})
	if err!=nil{return nil,err}
	out:=make([]corev1.Pod,0,len(pods.Items))
	for _,p:=range pods.Items {
		if p.DeletionTimestamp==nil {out=append(out,p)}
	}
	sort.Slice(out,func(i,j int)bool{return string(out[i].UID)<string(out[j].UID)})
	return out,nil
}

func waitForLiveCondition(t *testing.T,desc string,fn func() (bool,error)){
	t.Helper()
	until:=time.Now().Add(3*time.Minute)
	var lastErr error
	for time.Now().Before(until) {
		ok,err:=fn()
		if err==nil && ok {return}
		if err!=nil {lastErr=err}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out waiting for %s (last error: %v)",desc,lastErr)
}

func TestLiveDeploymentSurgeSlotConvergesAfterRollingUpdate(t *testing.T){
	a,dep:=liveSlotTestAdapter(t,1)
	ctx:=context.Background()
	var original corev1.Pod
	waitForLiveCondition(t,"original Running Pod",func()(bool,error){
		pods,err:=livePods(ctx,a)
		if err!=nil{return false,err}
		if len(pods)!=1 || pods[0].Status.Phase!=corev1.PodRunning{return false,nil}
		original=pods[0]
		return true,nil
	})
	if _,err:=a.ensureSwarmSlotAssignments(ctx,dep,[]corev1.Pod{original});err!=nil {
		t.Fatalf("initial slot assignment: %v",err)
	}
	if err:=a.setPodHealthCondition(ctx,original.Name,corev1.ConditionTrue,"TestHealthy","healthy");err!=nil {
		t.Fatalf("open initial health readiness gate: %v",err)
	}
	waitForLiveCondition(t,"original Deployment availability",func()(bool,error){
		d,err:=a.client.AppsV1().Deployments(a.namespace).Get(ctx,dep.Name,metav1.GetOptions{})
		if err!=nil{return false,err}
		return d.Status.AvailableReplicas==1,nil
	})

	current,err:=a.client.AppsV1().Deployments(a.namespace).Get(ctx,dep.Name,metav1.GetOptions{})
	if err!=nil {t.Fatalf("get Deployment before update: %v",err)}
	newVersion:=current.DeepCopy()
	if newVersion.Spec.Template.Annotations==nil{newVersion.Spec.Template.Annotations=map[string]string{}}
	newVersion.Spec.Template.Annotations["d2k-prr-test-rollout"]="updated"
	if _,err:=a.client.AppsV1().Deployments(a.namespace).Update(ctx,newVersion,metav1.UpdateOptions{});err!=nil{
		t.Fatalf("start RollingUpdate: %v",err)
	}

	var surge corev1.Pod
	waitForLiveCondition(t,"overlapping rollout surge Pod",func()(bool,error){
		pods,err:=livePods(ctx,a)
		if err!=nil{return false,err}
		if len(pods)<2{return false,nil}
		for _,pod:=range pods {
			if pod.UID!=original.UID && pod.Status.Phase==corev1.PodRunning {
				surge=pod
				return true,nil
			}
		}
		return false,nil
	})
	pods,err:=livePods(ctx,a)
	if err!=nil {t.Fatalf("list overlapping Pods: %v",err)}
	state,err:=a.ensureSwarmSlotAssignments(ctx,dep,pods)
	if err!=nil{t.Fatalf("assign overlapping Pods: %v",err)}
	if slot:=state.slotForPodUID(string(original.UID));slot==nil || slot.Slot!=1 {
		t.Fatalf("original slot was stolen during surge: %#v",state.Slots)
	}
	if err:=a.setPodHealthCondition(ctx,surge.Name,corev1.ConditionTrue,"TestHealthy","healthy");err!=nil {
		t.Fatalf("open surge health gate: %v",err)
	}

	waitForLiveCondition(t,"old Pod retirement after surge readiness",func()(bool,error){
		pods,err:=livePods(ctx,a)
		if err!=nil{return false,err}
		return len(pods)==1 && pods[0].UID==surge.UID,nil
	})
	pods,err=livePods(ctx,a)
	if err!=nil{t.Fatalf("get surviving Pod: %v",err)}
	state,err=a.ensureSwarmSlotAssignments(ctx,dep,pods)
	if err!=nil{t.Fatalf("reconcile surviving slot: %v",err)}
	slot:=state.slotForPodUID(string(surge.UID))
	if slot==nil || slot.Slot!=1 {
		t.Fatalf("real rollout survivor is stranded outside slot 1: %#v",state.Slots)
	}
	updatedPod,err:=a.client.CoreV1().Pods(a.namespace).Get(ctx,surge.Name,metav1.GetOptions{})
	if err!=nil {t.Fatalf("fetch promoted Pod: %v",err)}
	if updatedPod.Labels[types.LabelSwarmSlot]!="1" {
		t.Fatalf("live promoted Pod slot label = %q, want 1",updatedPod.Labels[types.LabelSwarmSlot])
	}
	tasks,err:=a.SwarmListTasks(ctx,"web","")
	if err!=nil {t.Fatalf("Swarm task readback: %v",err)}
	if len(tasks)!=1 || tasks[0]["ID"]!=slot.CurrentTaskID {
		t.Fatalf("task readback returned synthetic/incorrect task: %#v",tasks)
	}
}
