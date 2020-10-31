/*
Copyright 2019 The HAProxy Ingress Controller Authors.

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

package controller

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	api "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"github.com/jcmoraisjr/haproxy-ingress/pkg/acme"
	cfile "github.com/jcmoraisjr/haproxy-ingress/pkg/common/file"
	"github.com/jcmoraisjr/haproxy-ingress/pkg/common/ingress/controller"
	"github.com/jcmoraisjr/haproxy-ingress/pkg/common/net/ssl"
	convtypes "github.com/jcmoraisjr/haproxy-ingress/pkg/converters/types"
	"github.com/jcmoraisjr/haproxy-ingress/pkg/types"
	"github.com/jcmoraisjr/haproxy-ingress/pkg/utils"
)

const dhparamFilename = "dhparam.pem"

type k8scache struct {
	ctx                    context.Context
	client                 k8s.Interface
	listers                *listers
	controller             *controller.GenericController
	tracker                convtypes.Tracker
	crossNS                bool
	globalConfigMapKey     string
	tcpConfigMapKey        string
	acmeSecretKeyName      string
	acmeTokenConfigmapName string
	//
	updateQueue      utils.Queue
	stateMutex       sync.RWMutex
	waitBeforeUpdate time.Duration
	clear            bool
	needFullSync     bool
	//
	globalConfigMapData    map[string]string
	tcpConfigMapData       map[string]string
	globalConfigMapDataNew map[string]string
	tcpConfigMapDataNew    map[string]string
	//
	ingressesDel []*networking.Ingress
	ingressesUpd []*networking.Ingress
	ingressesAdd []*networking.Ingress
	endpointsNew []*api.Endpoints
	servicesDel  []*api.Service
	servicesUpd  []*api.Service
	servicesAdd  []*api.Service
	secretsDel   []*api.Secret
	secretsUpd   []*api.Secret
	secretsAdd   []*api.Secret
	podsNew      []*api.Pod
	//
}

func createCache(
	logger types.Logger,
	client k8s.Interface,
	controller *controller.GenericController,
	tracker convtypes.Tracker,
	updateQueue utils.Queue,
	watchNamespace string,
	isolateNamespace bool,
	disablePodList bool,
	resync time.Duration,
	waitBeforeUpdate time.Duration,
) *k8scache {
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		// TODO implement a smart fallback or error checking
		// Fallback to a valid name if envvar is not provided. Should never be used because:
		// - `namespace` is only used in `acme*`
		// - `acme*` is only used by acme client and server
		// - acme client and server are only used if leader elector is enabled
		// - leader elector will panic if this envvar is not provided
		namespace = "default"
	}
	cfg := controller.GetConfig()
	acmeSecretKeyName := cfg.AcmeSecretKeyName
	if !strings.Contains(acmeSecretKeyName, "/") {
		acmeSecretKeyName = namespace + "/" + acmeSecretKeyName
	}
	acmeTokenConfigmapName := cfg.AcmeTokenConfigmapName
	if !strings.Contains(acmeTokenConfigmapName, "/") {
		acmeTokenConfigmapName = namespace + "/" + acmeTokenConfigmapName
	}
	globalConfigMapName := cfg.ConfigMapName
	tcpConfigMapName := cfg.TCPConfigMapName
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(logger.Info)
	eventBroadcaster.StartRecordingToSink(&typedv1.EventSinkImpl{
		Interface: client.CoreV1().Events(watchNamespace),
	})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, api.EventSource{
		Component: "ingress-controller",
	})
	cache := &k8scache{
		ctx:                    context.Background(),
		client:                 client,
		controller:             controller,
		tracker:                tracker,
		crossNS:                cfg.AllowCrossNamespace,
		globalConfigMapKey:     globalConfigMapName,
		tcpConfigMapKey:        tcpConfigMapName,
		acmeSecretKeyName:      acmeSecretKeyName,
		acmeTokenConfigmapName: acmeTokenConfigmapName,
		stateMutex:             sync.RWMutex{},
		updateQueue:            updateQueue,
		waitBeforeUpdate:       waitBeforeUpdate,
		clear:                  true,
		needFullSync:           false,
	}
	// TODO I'm a circular reference, can you fix me?
	cache.listers = createListers(cache, logger, recorder, client, watchNamespace, isolateNamespace, !disablePodList, resync)
	return cache
}

func (c *k8scache) RunAsync(stopCh <-chan struct{}) {
	c.listers.RunAsync(stopCh)
}

func (c *k8scache) GetIngressPodName() (namespace, podname string, err error) {
	namespace = os.Getenv("POD_NAMESPACE")
	podname = os.Getenv("POD_NAME")
	if namespace == "" || podname == "" {
		return "", "", fmt.Errorf("missing POD_NAMESPACE or POD_NAME envvar")
	}
	if pod, _ := c.client.CoreV1().Pods(namespace).Get(c.ctx, podname, metav1.GetOptions{}); pod == nil {
		return "", "", fmt.Errorf("ingress controller pod was not found: %s/%s", namespace, podname)
	}
	return namespace, podname, nil
}

func (c *k8scache) GetIngress(ingressName string) (*networking.Ingress, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(ingressName)
	if err != nil {
		return nil, err
	}
	ing, err := c.listers.ingressLister.Ingresses(namespace).Get(name)
	if ing != nil && !c.IsValidIngress(ing) {
		return nil, fmt.Errorf("ingress class does not match")
	}
	return ing, err
}

func (c *k8scache) GetIngressList() ([]*networking.Ingress, error) {
	ingList, err := c.listers.ingressLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	validIngList := make([]*networking.Ingress, len(ingList))
	var i int
	for _, ing := range ingList {
		if c.IsValidIngress(ing) {
			validIngList[i] = ing
			i++
		}
	}
	return validIngList[:i], nil
}

func (c *k8scache) GetService(serviceName string) (*api.Service, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(serviceName)
	if err != nil {
		return nil, err
	}
	return c.listers.serviceLister.Services(namespace).Get(name)
}

func (c *k8scache) GetSecret(secretName string) (*api.Secret, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(secretName)
	if err != nil {
		return nil, err
	}
	if c.listers.running {
		return c.listers.secretLister.Secrets(namespace).Get(name)
	}
	return c.client.CoreV1().Secrets(namespace).Get(c.ctx, name, metav1.GetOptions{})
}

func (c *k8scache) GetConfigMap(configMapName string) (*api.ConfigMap, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(configMapName)
	if err != nil {
		return nil, err
	}
	return c.listers.configMapLister.ConfigMaps(namespace).Get(name)
}

func (c *k8scache) GetEndpoints(service *api.Service) (*api.Endpoints, error) {
	return c.listers.endpointLister.Endpoints(service.Namespace).Get(service.Name)
}

// GetTerminatingPods returns the pods that are terminating and belong
// (based on the Spec.Selector) to the supplied service.
func (c *k8scache) GetTerminatingPods(service *api.Service, track convtypes.TrackingTarget) (pl []*api.Pod, err error) {
	if !c.listers.hasPodLister {
		return nil, fmt.Errorf("pod lister wasn't started, remove --disable-pod-list command-line option to enable it")
	}
	// converting the service selector to slice of string
	// in order to create the full match selector
	var ls []string
	for k, v := range service.Spec.Selector {
		ls = append(ls, fmt.Sprintf("%s=%s", k, v))
	}
	// parsing the label selector from the previous selectors
	l, err := labels.Parse(strings.Join(ls, ","))
	if err != nil {
		return nil, err
	}
	list, err := c.listers.podLister.Pods(service.Namespace).List(l)
	if err != nil {
		return nil, err
	}
	for _, p := range list {
		// all pods need to be tracked despite of the terminating status
		c.tracker.Track(false, track, convtypes.PodType, p.Namespace+"/"+p.Name)
		if isTerminatingPod(service, p) {
			pl = append(pl, p)
		}
	}
	return pl, nil
}

// isTerminatingPod Indicates whether or not pod belongs to svc, and is in the process of terminating
func isTerminatingPod(svc *api.Service, pod *api.Pod) bool {
	if svc.GetNamespace() != pod.GetNamespace() {
		return false
	}
	for selectorLabel, selectorValue := range svc.Spec.Selector {
		if labelValue, present := pod.Labels[selectorLabel]; !present || selectorValue != labelValue {
			return false
		}
	}
	if pod.DeletionTimestamp != nil && pod.Status.Reason != "NodeLost" && pod.Status.PodIP != "" {
		return true
	}
	return false
}

func (c *k8scache) GetPod(podName string) (*api.Pod, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(podName)
	if err != nil {
		return nil, err
	}
	if c.listers.hasPodLister {
		return c.listers.podLister.Pods(namespace).Get(name)
	}
	// A fallback just in case --disable-pod-list is configured.
	return c.client.CoreV1().Pods(namespace).Get(c.ctx, name, metav1.GetOptions{})
}

func (c *k8scache) buildSecretName(defaultNamespace, secretName string) (string, string, error) {
	ns, name, err := cache.SplitMetaNamespaceKey(secretName)
	if err != nil {
		return "", "", err
	}
	if defaultNamespace == "" {
		return ns, name, nil
	}
	if ns == "" {
		return defaultNamespace, name, nil
	}
	if c.crossNS || ns == defaultNamespace {
		return ns, name, nil
	}
	return "", "", fmt.Errorf(
		"trying to read secret '%s' from namespace '%s', but cross-namespace reading is disabled; use --allow-cross-namespace to enable",
		secretName, defaultNamespace,
	)
}

func (c *k8scache) GetTLSSecretPath(defaultNamespace, secretName string, track convtypes.TrackingTarget) (file convtypes.CrtFile, err error) {
	namespace, name, err := c.buildSecretName(defaultNamespace, secretName)
	if err != nil {
		return file, err
	}
	sslCert, err := c.controller.GetCertificate(namespace, name)
	if err != nil {
		c.tracker.Track(true, track, convtypes.SecretType, namespace+"/"+name)
		return file, err
	}
	if sslCert.PemFileName == "" {
		c.tracker.Track(true, track, convtypes.SecretType, namespace+"/"+name)
		return file, fmt.Errorf("secret '%s/%s' does not have keys 'tls.crt' and 'tls.key'", namespace, name)
	}
	file = convtypes.CrtFile{
		Filename:   sslCert.PemFileName,
		SHA1Hash:   sslCert.PemSHA,
		CommonName: sslCert.Certificate.Subject.CommonName,
		NotAfter:   sslCert.Certificate.NotAfter,
	}
	c.tracker.Track(false, track, convtypes.SecretType, namespace+"/"+name)
	return file, nil
}

func (c *k8scache) GetCASecretPath(defaultNamespace, secretName string, track convtypes.TrackingTarget) (ca, crl convtypes.File, err error) {
	namespace, name, err := c.buildSecretName(defaultNamespace, secretName)
	if err != nil {
		return ca, crl, err
	}
	sslCert, err := c.controller.GetCertificate(namespace, name)
	if err != nil {
		c.tracker.Track(true, track, convtypes.SecretType, namespace+"/"+name)
		return ca, crl, err
	}
	if sslCert.CAFileName == "" {
		c.tracker.Track(true, track, convtypes.SecretType, namespace+"/"+name)
		return ca, crl, fmt.Errorf("secret '%s/%s' does not have key 'ca.crt'", namespace, name)
	}
	ca = convtypes.File{
		Filename: sslCert.CAFileName,
		SHA1Hash: sslCert.PemSHA,
	}
	if sslCert.CRLFileName != "" {
		// ssl.AddCertAuth concatenates the hash of CA and CRL into the same attribute
		crl = convtypes.File{
			Filename: sslCert.CRLFileName,
			SHA1Hash: sslCert.PemSHA,
		}
	}
	c.tracker.Track(false, track, convtypes.SecretType, namespace+"/"+name)
	return ca, crl, nil
}

func (c *k8scache) GetDHSecretPath(defaultNamespace, secretName string) (file convtypes.File, err error) {
	namespace, name, err := c.buildSecretName(defaultNamespace, secretName)
	if err != nil {
		return file, err
	}
	secret, err := c.listers.secretLister.Secrets(namespace).Get(name)
	if err != nil {
		return file, err
	}
	dh, found := secret.Data[dhparamFilename]
	if !found {
		return file, fmt.Errorf("secret '%s/%s' does not have key '%s'", namespace, name, dhparamFilename)
	}
	pem := fmt.Sprintf("%s_%s", namespace, name)
	pemFileName, err := ssl.AddOrUpdateDHParam(pem, dh)
	if err != nil {
		return file, fmt.Errorf("error creating dh-param file '%s': %v", pem, err)
	}
	file = convtypes.File{
		Filename: pemFileName,
		SHA1Hash: cfile.SHA1(pemFileName),
	}
	return file, nil
}

func (c *k8scache) GetSecretContent(defaultNamespace, secretName, keyName string, track convtypes.TrackingTarget) ([]byte, error) {
	namespace, name, err := c.buildSecretName(defaultNamespace, secretName)
	if err != nil {
		return nil, err
	}
	secret, err := c.listers.secretLister.Secrets(namespace).Get(name)
	if err != nil {
		c.tracker.Track(true, track, convtypes.SecretType, namespace+"/"+name)
		return nil, err
	}
	data, found := secret.Data[keyName]
	if !found {
		c.tracker.Track(true, track, convtypes.SecretType, namespace+"/"+name)
		return nil, fmt.Errorf("secret '%s/%s' does not have key '%s'", namespace, name, keyName)
	}
	c.tracker.Track(false, track, convtypes.SecretType, namespace+"/"+name)
	return data, nil
}

// Implements acme.ClientResolver
func (c *k8scache) GetKey() (crypto.Signer, error) {
	secret, err := c.GetSecret(c.acmeSecretKeyName)
	var key *rsa.PrivateKey
	if err == nil {
		pemKey, found := secret.Data[api.TLSPrivateKeyKey]
		if !found {
			return nil, fmt.Errorf("secret '%s' does not have a key", c.acmeSecretKeyName)
		}
		derBlock, _ := pem.Decode(pemKey)
		if derBlock == nil {
			return nil, fmt.Errorf("secret '%s' has not a valid pem encoded private key", c.acmeSecretKeyName)
		}
		key, err = x509.ParsePKCS1PrivateKey(derBlock.Bytes)
		if err != nil {
			return nil, fmt.Errorf("error parsing acme client private key: %v", err)
		}
	}
	if key == nil {
		namespace, name, err := cache.SplitMetaNamespaceKey(c.acmeSecretKeyName)
		if err != nil {
			return nil, err
		}
		key, err = rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, err
		}
		pemEncode := pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(key),
		})
		newSecret := &api.Secret{}
		newSecret.Namespace = namespace
		newSecret.Name = name
		newSecret.Data = map[string][]byte{api.TLSPrivateKeyKey: pemEncode}
		if err := c.CreateOrUpdateSecret(newSecret); err != nil {
			return nil, err
		}
	}
	return key, nil
}

// Implements acme.SignerResolver
func (c *k8scache) GetTLSSecretContent(secretName string) (*acme.TLSSecret, error) {
	secret, err := c.GetSecret(secretName)
	if err != nil {
		return nil, err
	}
	pemCrt, foundCrt := secret.Data[api.TLSCertKey]
	pemKey, foundKey := secret.Data[api.TLSPrivateKeyKey]
	if !foundCrt {
		return nil, fmt.Errorf("secret %s does not have %s key", secretName, api.TLSCertKey)
	}
	if !foundKey {
		return nil, fmt.Errorf("secret %s does not have %s key", secretName, api.TLSPrivateKeyKey)
	}
	derCrt, _ := pem.Decode(pemCrt)
	derKey, _ := pem.Decode(pemKey)
	if derCrt == nil {
		return nil, fmt.Errorf("error decoding crt of secret %s: cannot find a proper pem block", secretName)
	}
	if derKey == nil {
		return nil, fmt.Errorf("error decoding key of secret %s: cannot find a proper pem block", secretName)
	}
	crt, errCrt := x509.ParseCertificate(derCrt.Bytes)
	key, errKey := x509.ParsePKCS1PrivateKey(derKey.Bytes)
	if errCrt != nil {
		return nil, fmt.Errorf("error parsing crt of secret %s: %w", secretName, errCrt)
	}
	if errKey != nil {
		return nil, fmt.Errorf("error parsing key of secret %s: %w", secretName, errKey)
	}
	return &acme.TLSSecret{
		Crt: crt,
		Key: key,
	}, nil
}

// Implements acme.SignerResolver
func (c *k8scache) SetTLSSecretContent(secretName string, pemCrt, pemKey []byte) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(secretName)
	if err != nil {
		return err
	}
	secret := &api.Secret{}
	secret.Namespace = namespace
	secret.Name = name
	secret.Type = api.SecretTypeTLS
	secret.Data = map[string][]byte{
		api.TLSCertKey:       pemCrt,
		api.TLSPrivateKeyKey: pemKey,
	}
	return c.CreateOrUpdateSecret(secret)
}

// Implements acme.ServerResolver
func (c *k8scache) GetToken(domain, uri string) string {
	config, err := c.GetConfigMap(c.acmeTokenConfigmapName)
	if err != nil {
		return ""
	}
	data, found := config.Data[domain]
	if !found {
		return ""
	}
	prefix := uri + "="
	if !strings.HasPrefix(data, prefix) {
		return ""
	}
	return strings.TrimPrefix(data, prefix)
}

// Implements acme.ClientResolver
func (c *k8scache) SetToken(domain string, uri, token string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(c.acmeTokenConfigmapName)
	if err != nil {
		return err
	}
	config, err := c.listers.configMapLister.ConfigMaps(namespace).Get(name)
	if err != nil {
		config = &api.ConfigMap{}
		config.Namespace = namespace
		config.Name = name
	}
	if config.Data == nil {
		config.Data = make(map[string]string, 1)
	}
	if token != "" {
		config.Data[domain] = uri + "=" + token
	} else {
		delete(config.Data, domain)
	}
	return c.CreateOrUpdateConfigMap(config)
}

func (c *k8scache) CreateOrUpdateSecret(secret *api.Secret) (err error) {
	cli := c.client.CoreV1().Secrets(secret.Namespace)
	if _, err := c.listers.secretLister.Secrets(secret.Namespace).Get(secret.Name); err != nil {
		_, err = cli.Create(c.ctx, secret, metav1.CreateOptions{})
	} else {
		_, err = cli.Update(c.ctx, secret, metav1.UpdateOptions{})
	}
	return err
}

func (c *k8scache) CreateOrUpdateConfigMap(cm *api.ConfigMap) (err error) {
	cli := c.client.CoreV1().ConfigMaps(cm.Namespace)
	if _, err := c.listers.configMapLister.ConfigMaps(cm.Namespace).Get(cm.Name); err != nil {
		_, err = cli.Create(c.ctx, cm, metav1.CreateOptions{})
	} else {
		_, err = cli.Update(c.ctx, cm, metav1.UpdateOptions{})
	}
	return err
}

// implements ListerEvents
func (c *k8scache) IsValidIngress(ing *networking.Ingress) bool {
	return c.controller.IsValidClass(ing)
}

// implements ListerEvents
func (c *k8scache) IsValidConfigMap(cm *api.ConfigMap) bool {
	key := fmt.Sprintf("%s/%s", cm.Namespace, cm.Name)
	return key == c.globalConfigMapKey || key == c.tcpConfigMapKey
}

// implements ListerEvents
func (c *k8scache) Notify(old, cur interface{}) {
	// IMPLEMENT
	// maintain a list of changed objects only if partial parsing is being used
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()
	// old != nil: has the `old` state of a changed or removed object
	// cur != nil: has the `cur` state of a changed or a just created object
	// old and cur == nil: cannot identify what was changed, need to start a full resync
	if old != nil {
		switch old.(type) {
		case *networking.Ingress:
			if cur == nil {
				c.ingressesDel = append(c.ingressesDel, old.(*networking.Ingress))
			}
		case *api.Service:
			if cur == nil {
				c.servicesDel = append(c.servicesDel, old.(*api.Service))
			}
		case *api.Secret:
			if cur == nil {
				secret := old.(*api.Secret)
				c.secretsDel = append(c.secretsDel, secret)
				c.controller.DeleteSecret(fmt.Sprintf("%s/%s", secret.Namespace, secret.Name))
			}
		}
	}
	if cur != nil {
		switch cur.(type) {
		case *networking.Ingress:
			ing := cur.(*networking.Ingress)
			if old == nil {
				c.ingressesAdd = append(c.ingressesAdd, ing)
			} else {
				c.ingressesUpd = append(c.ingressesUpd, ing)
			}
		case *api.Endpoints:
			c.endpointsNew = append(c.endpointsNew, cur.(*api.Endpoints))
		case *api.Service:
			svc := cur.(*api.Service)
			if old == nil {
				c.servicesAdd = append(c.servicesAdd, svc)
			} else {
				c.servicesUpd = append(c.servicesUpd, svc)
			}
		case *api.Secret:
			secret := cur.(*api.Secret)
			if old == nil {
				c.secretsAdd = append(c.secretsAdd, secret)
			} else {
				c.secretsUpd = append(c.secretsUpd, secret)
			}
			c.controller.UpdateSecret(fmt.Sprintf("%s/%s", secret.Namespace, secret.Name))
		case *api.ConfigMap:
			cm := cur.(*api.ConfigMap)
			key := fmt.Sprintf("%s/%s", cm.Namespace, cm.Name)
			switch key {
			case c.globalConfigMapKey:
				c.globalConfigMapDataNew = cm.Data
			case c.tcpConfigMapKey:
				c.tcpConfigMapDataNew = cm.Data
			}
		case *api.Pod:
			c.podsNew = append(c.podsNew, cur.(*api.Pod))
		}
	}
	if old == nil && cur == nil {
		c.needFullSync = true
	}
	if c.clear {
		// Wait before notify, giving the time to receive
		// all/most of the changes of a batch update
		time.AfterFunc(c.waitBeforeUpdate, func() { c.updateQueue.Notify() })
	}
	c.clear = false
}

// implements converters.types.Cache
func (c *k8scache) SwapChangedObjects() *convtypes.ChangedObjects {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()
	//
	var obj []string
	if c.globalConfigMapDataNew != nil && !reflect.DeepEqual(c.globalConfigMapData, c.globalConfigMapDataNew) {
		obj = append(obj, "update/global")
	}
	if c.tcpConfigMapDataNew != nil && !reflect.DeepEqual(c.tcpConfigMapData, c.tcpConfigMapDataNew) {
		obj = append(obj, "update/tcp-services")
	}
	for _, ing := range c.ingressesDel {
		obj = append(obj, "del/ingress:"+ing.Namespace+"/"+ing.Name)
	}
	for _, ing := range c.ingressesUpd {
		obj = append(obj, "update/ingress:"+ing.Namespace+"/"+ing.Name)
	}
	for _, ing := range c.ingressesAdd {
		obj = append(obj, "add/ingress:"+ing.Namespace+"/"+ing.Name)
	}
	for _, ep := range c.endpointsNew {
		obj = append(obj, "update/endpoint:"+ep.Namespace+"/"+ep.Name)
	}
	for _, svc := range c.servicesDel {
		obj = append(obj, "del/service:"+svc.Namespace+"/"+svc.Name)
	}
	for _, svc := range c.servicesUpd {
		obj = append(obj, "update/service:"+svc.Namespace+"/"+svc.Name)
	}
	for _, svc := range c.servicesAdd {
		obj = append(obj, "add/service:"+svc.Namespace+"/"+svc.Name)
	}
	for _, secret := range c.secretsDel {
		obj = append(obj, "del/secret:"+secret.Namespace+"/"+secret.Name)
	}
	for _, secret := range c.secretsUpd {
		obj = append(obj, "update/secret:"+secret.Namespace+"/"+secret.Name)
	}
	for _, secret := range c.secretsAdd {
		obj = append(obj, "add/secret:"+secret.Namespace+"/"+secret.Name)
	}
	for _, pod := range c.podsNew {
		obj = append(obj, "update/pod:"+pod.Namespace+"/"+pod.Name)
	}
	//
	changed := &convtypes.ChangedObjects{
		GlobalCur:       c.globalConfigMapData,
		GlobalNew:       c.globalConfigMapDataNew,
		TCPConfigMapCur: c.tcpConfigMapData,
		TCPConfigMapNew: c.tcpConfigMapDataNew,
		IngressesDel:    c.ingressesDel,
		IngressesUpd:    c.ingressesUpd,
		IngressesAdd:    c.ingressesAdd,
		Endpoints:       c.endpointsNew,
		ServicesDel:     c.servicesDel,
		ServicesUpd:     c.servicesUpd,
		ServicesAdd:     c.servicesAdd,
		SecretsDel:      c.secretsDel,
		SecretsUpd:      c.secretsUpd,
		SecretsAdd:      c.secretsAdd,
		Pods:            c.podsNew,
		Objects:         obj,
	}
	//
	c.podsNew = nil
	c.endpointsNew = nil
	//
	// Secrets
	//
	c.secretsDel = nil
	c.secretsUpd = nil
	c.secretsAdd = nil
	//
	// Services
	//
	c.servicesDel = nil
	c.servicesUpd = nil
	c.servicesAdd = nil
	//
	// Ingress
	//
	c.ingressesDel = nil
	c.ingressesUpd = nil
	c.ingressesAdd = nil
	//
	// ConfigMaps
	//
	if c.globalConfigMapDataNew != nil {
		c.globalConfigMapData = c.globalConfigMapDataNew
		c.globalConfigMapDataNew = nil
	}
	if c.tcpConfigMapDataNew != nil {
		c.tcpConfigMapData = c.tcpConfigMapDataNew
		c.tcpConfigMapDataNew = nil
	}
	//
	c.clear = true
	c.needFullSync = false
	return changed
}

// implements converters.types.Cache
func (c *k8scache) NeedFullSync() bool {
	c.stateMutex.RLock()
	defer c.stateMutex.RUnlock()
	return c.needFullSync
}
