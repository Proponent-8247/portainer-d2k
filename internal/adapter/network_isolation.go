package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/portainer/d2k/internal/types"
	"github.com/portainer/d2k/pkg/portmapper"
)

func splitCIDRs(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" { out = append(out, v) }
	}
	return out
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:8])
}

func networkLabelKey(id string) string { return types.LabelNetworkPrefix + shortHash(id) }
func networkStateName(id string) string { return "d2k-network-" + shortHash(id) }
func networkPolicyName(id string) string { return "d2k-net-" + shortHash(id) }
func publishedPolicyName(name string) string { return "d2k-published-" + shortHash(name) }

func encodeNetworkIDs(ids []string) string {
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	b, _ := json.Marshal(ids)
	return string(b)
}

func decodeNetworkIDs(raw string) []string {
	var ids []string
	_ = json.Unmarshal([]byte(raw), &ids)
	return ids
}

func (a *KubernetesDockerAdapter) ensureIsolationBaseline(ctx context.Context) error {
	if !a.networkIsolation { return nil }
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name:"d2k-workloads-default-deny", Namespace:a.namespace, Labels:map[string]string{types.LabelManagedBy:types.LabelManagedByValue}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels:map[string]string{types.LabelManagedBy:types.LabelManagedByValue}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	return a.applyNetworkPolicy(ctx,np)
}

func (a *KubernetesDockerAdapter) persistNetwork(ctx context.Context, n *NetworkSummary) error {
	if !a.networkIsolation || n == nil { return nil }
	labelsJSON, _ := json.Marshal(n.Labels)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name:networkStateName(n.ID), Namespace:a.namespace, Labels:map[string]string{types.LabelNetworkState:"true",types.LabelManagedBy:types.LabelManagedByValue}},
		Data: map[string]string{"id":n.ID,"name":n.Name,"driver":n.Driver,"scope":n.Scope,"internal":strconv.FormatBool(n.Internal),"attachable":strconv.FormatBool(n.Attachable),"labels":string(labelsJSON)},
	}
	api:=a.client.CoreV1().ConfigMaps(a.namespace)
	current,err:=api.Get(ctx,cm.Name,metav1.GetOptions{})
	if apierrors.IsNotFound(err) { _,err=api.Create(ctx,cm,metav1.CreateOptions{}); return err }
	if err != nil { return err }
	cm.ResourceVersion=current.ResourceVersion
	_,err=api.Update(ctx,cm,metav1.UpdateOptions{})
	return err
}

func (a *KubernetesDockerAdapter) deletePersistedNetwork(ctx context.Context, n *NetworkSummary) error {
	if n == nil { return nil }
	err:=a.client.CoreV1().ConfigMaps(a.namespace).Delete(ctx,networkStateName(n.ID),metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) { return nil }
	return err
}

func (a *KubernetesDockerAdapter) restorePersistedNetworks(ctx context.Context) error {
	if !a.networkIsolation { return nil }
	cms,err:=a.client.CoreV1().ConfigMaps(a.namespace).List(ctx,metav1.ListOptions{LabelSelector:types.LabelNetworkState+"=true"})
	if err != nil { return err }
	restored:=map[string]*NetworkSummary{}
	for _,cm:=range cms.Items {
		if cm.Data["id"]=="" || cm.Data["name"]=="" { continue }
		var labels map[string]string
		_ = json.Unmarshal([]byte(cm.Data["labels"]), &labels)
		internal,_:=strconv.ParseBool(cm.Data["internal"])
		attachable,_:=strconv.ParseBool(cm.Data["attachable"])
		restored[cm.Data["name"]]=&NetworkSummary{ID:cm.Data["id"],Name:cm.Data["name"],Driver:cm.Data["driver"],Scope:cm.Data["scope"],Internal:internal,Attachable:attachable,IPAM:NetworkIPAM{Driver:"default"},Labels:labels}
	}
	a.networksMu.Lock()
	for name,n:=range restored { a.networks[name]=n }
	a.networksMu.Unlock()
	return nil
}

func (a *KubernetesDockerAdapter) findNetwork(ctx context.Context, ref string) (*NetworkSummary,error) {
	networks,err:=a.ListNetworks(ctx)
	if err != nil { return nil,err }
	for i:=range networks {
		if networks[i].Name==ref || networks[i].ID==ref {
			n:=networks[i]
			return &n,nil
		}
	}
	return nil,fmt.Errorf("Docker network %q does not exist",ref)
}

func (a *KubernetesDockerAdapter) networkMembership(ctx context.Context, refs []string, defaultRef string) (map[string]string,[]string,error) {
	if len(refs)==0 && defaultRef!="" { refs=[]string{defaultRef} }
	labels:=map[string]string{}
	var ids []string
	seen:=map[string]bool{}
	for _,ref:=range refs {
		n,err:=a.findNetwork(ctx,ref)
		if err!=nil { return nil,nil,err }
		if seen[n.ID] { continue }
		seen[n.ID]=true
		if err:=a.ensureNetworkIsolationPolicy(ctx,n); err!=nil { return nil,nil,err }
		labels[networkLabelKey(n.ID)]="true"
		ids=append(ids,n.ID)
	}
	sort.Strings(ids)
	return labels,ids,nil
}

func stripNetworkLabels(labels map[string]string) {
	for key:=range labels {
		if strings.HasPrefix(key,types.LabelNetworkPrefix) { delete(labels,key) }
	}
}

func (a *KubernetesDockerAdapter) applyDeploymentNetworks(ctx context.Context, dep *appsv1.Deployment, refs []string, defaultRef string) error {
	labels,ids,err:=a.networkMembership(ctx,refs,defaultRef)
	if err!=nil { return err }
	if dep.Labels==nil { dep.Labels=map[string]string{} }
	if dep.Annotations==nil { dep.Annotations=map[string]string{} }
	if dep.Spec.Template.Labels==nil { dep.Spec.Template.Labels=map[string]string{} }
	if dep.Spec.Template.Annotations==nil { dep.Spec.Template.Annotations=map[string]string{} }
	stripNetworkLabels(dep.Labels)
	stripNetworkLabels(dep.Spec.Template.Labels)
	for k,v:=range labels { dep.Labels[k]=v; dep.Spec.Template.Labels[k]=v }
	raw:=encodeNetworkIDs(ids)
	dep.Annotations[types.AnnotationNetworkIDs]=raw
	dep.Spec.Template.Annotations[types.AnnotationNetworkIDs]=raw
	return nil
}

func (a *KubernetesDockerAdapter) sameNetworkPeer(id string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{PodSelector:&metav1.LabelSelector{MatchLabels:map[string]string{networkLabelKey(id):"true"}}}
}

func (a *KubernetesDockerAdapter) dnsEgressRule() networkingv1.NetworkPolicyEgressRule {
	tcp:=corev1.ProtocolTCP; udp:=corev1.ProtocolUDP; port:=intstr.FromInt(53)
	return networkingv1.NetworkPolicyEgressRule{
		To:[]networkingv1.NetworkPolicyPeer{{NamespaceSelector:&metav1.LabelSelector{MatchLabels:map[string]string{"kubernetes.io/metadata.name":"kube-system"}},PodSelector:&metav1.LabelSelector{MatchLabels:map[string]string{"k8s-app":"kube-dns"}}}},
		Ports:[]networkingv1.NetworkPolicyPort{{Protocol:&udp,Port:&port},{Protocol:&tcp,Port:&port}},
	}
}

func familyCIDRs(cidrs []string,wantV6 bool) []string {
	var out []string
	for _,raw:=range cidrs {
		ip,_,err:=net.ParseCIDR(raw); if err!=nil { continue }
		if (ip.To4()==nil)==wantV6 { out=append(out,raw) }
	}
	return out
}

func (a *KubernetesDockerAdapter) worldPeers() []networkingv1.NetworkPolicyPeer {
	all:=append(append([]string{},a.podCIDRs...),a.serviceCIDRs...)
	return []networkingv1.NetworkPolicyPeer{
		{IPBlock:&networkingv1.IPBlock{CIDR:"0.0.0.0/0",Except:familyCIDRs(all,false)}},
		{IPBlock:&networkingv1.IPBlock{CIDR:"::/0",Except:familyCIDRs(all,true)}},
	}
}

func (a *KubernetesDockerAdapter) ensureNetworkIsolationPolicy(ctx context.Context,n *NetworkSummary) error {
	if !a.networkIsolation || n==nil { return nil }
	key:=networkLabelKey(n.ID)
	spec:=networkingv1.NetworkPolicySpec{PodSelector:metav1.LabelSelector{MatchLabels:map[string]string{key:"true"}},PolicyTypes:[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress,networkingv1.PolicyTypeEgress}}
	if n.Name!="none" {
		spec.Ingress=[]networkingv1.NetworkPolicyIngressRule{{From:[]networkingv1.NetworkPolicyPeer{a.sameNetworkPeer(n.ID)}}}
		spec.Egress=[]networkingv1.NetworkPolicyEgressRule{{To:[]networkingv1.NetworkPolicyPeer{a.sameNetworkPeer(n.ID)}},a.dnsEgressRule()}
		if !n.Internal { spec.Egress=append(spec.Egress,networkingv1.NetworkPolicyEgressRule{To:a.worldPeers()}) }
	}
	np:=&networkingv1.NetworkPolicy{ObjectMeta:metav1.ObjectMeta{Name:networkPolicyName(n.ID),Namespace:a.namespace,Labels:map[string]string{types.LabelManagedBy:types.LabelManagedByValue}},Spec:spec}
	return a.applyNetworkPolicy(ctx,np)
}

func (a *KubernetesDockerAdapter) deleteNetworkIsolationPolicy(ctx context.Context,id string) error {
	err:=a.client.NetworkingV1().NetworkPolicies(a.namespace).Delete(ctx,networkPolicyName(id),metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) { return nil }
	return err
}

func (a *KubernetesDockerAdapter) applyNetworkPolicy(ctx context.Context,desired *networkingv1.NetworkPolicy) error {
	api:=a.client.NetworkingV1().NetworkPolicies(a.namespace)
	current,err:=api.Get(ctx,desired.Name,metav1.GetOptions{})
	if apierrors.IsNotFound(err) { _,err=api.Create(ctx,desired,metav1.CreateOptions{}); return err }
	if err!=nil { return err }
	desired.ResourceVersion=current.ResourceVersion
	_,err=api.Update(ctx,desired,metav1.UpdateOptions{})
	return err
}

func mappingsToNetworkPolicyPorts(mappings []portmapper.PortMapping) []networkingv1.NetworkPolicyPort {
	var ports []networkingv1.NetworkPolicyPort
	seen:=map[string]bool{}
	for _,m:=range mappings {
		proto:=corev1.ProtocolTCP
		if strings.EqualFold(string(m.Protocol),"UDP") { proto=corev1.ProtocolUDP }
		key:=fmt.Sprintf("%s/%d",proto,m.ContainerPort)
		if seen[key] { continue }; seen[key]=true
		p:=intstr.FromInt(m.ContainerPort)
		ports=append(ports,networkingv1.NetworkPolicyPort{Protocol:&proto,Port:&p})
	}
	return ports
}

func (a *KubernetesDockerAdapter) ensurePublishedIngressPolicy(ctx context.Context,name string,ports []networkingv1.NetworkPolicyPort) error {
	if !a.networkIsolation { return nil }
	if len(ports)==0 { return a.deletePublishedIngressPolicy(ctx,name) }
	np:=&networkingv1.NetworkPolicy{ObjectMeta:metav1.ObjectMeta{Name:publishedPolicyName(name),Namespace:a.namespace,Labels:map[string]string{types.LabelManagedBy:types.LabelManagedByValue}},Spec:networkingv1.NetworkPolicySpec{PodSelector:metav1.LabelSelector{MatchLabels:map[string]string{"app":name}},PolicyTypes:[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress},Ingress:[]networkingv1.NetworkPolicyIngressRule{{From:a.worldPeers(),Ports:ports}}}}
	return a.applyNetworkPolicy(ctx,np)
}

func (a *KubernetesDockerAdapter) deletePublishedIngressPolicy(ctx context.Context,name string) error {
	err:=a.client.NetworkingV1().NetworkPolicies(a.namespace).Delete(ctx,publishedPolicyName(name),metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) { return nil }
	return err
}

func (a *KubernetesDockerAdapter) ConnectNetwork(ctx context.Context,networkRef,container string) error {
	if !a.networkIsolation { return nil }
	resolved,err:=a.resolveDeploymentName(ctx,container); if err!=nil { return err }
	dep,err:=a.client.AppsV1().Deployments(a.namespace).Get(ctx,resolved,metav1.GetOptions{}); if err!=nil { return err }
	ids:=decodeNetworkIDs(dep.Annotations[types.AnnotationNetworkIDs])
	n,err:=a.findNetwork(ctx,networkRef); if err!=nil { return err }
	ids=append(ids,n.ID)
	if err:=a.applyDeploymentNetworks(ctx,dep,ids,""); err!=nil { return err }
	_,err=a.client.AppsV1().Deployments(a.namespace).Update(ctx,dep,metav1.UpdateOptions{})
	return err
}

func (a *KubernetesDockerAdapter) DisconnectNetwork(ctx context.Context,networkRef,container string) error {
	if !a.networkIsolation { return nil }
	resolved,err:=a.resolveDeploymentName(ctx,container); if err!=nil { return err }
	dep,err:=a.client.AppsV1().Deployments(a.namespace).Get(ctx,resolved,metav1.GetOptions{}); if err!=nil { return err }
	n,err:=a.findNetwork(ctx,networkRef); if err!=nil { return err }
	current:=decodeNetworkIDs(dep.Annotations[types.AnnotationNetworkIDs]); var ids []string
	for _,id:=range current { if id!=n.ID { ids=append(ids,id) } }
	if err:=a.applyDeploymentNetworks(ctx,dep,ids,""); err!=nil { return err }
	_,err=a.client.AppsV1().Deployments(a.namespace).Update(ctx,dep,metav1.UpdateOptions{})
	return err
}
