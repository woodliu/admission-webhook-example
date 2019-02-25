package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"github.com/golang/glog"
	"github.com/coreos/etcd/clientv3"
	"k8s.io/api/admission/v1beta1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/kubernetes/pkg/apis/core/v1"
	"time"
	"context"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
)

const (
	admissionWebhookAnnotationValidateKey = "admission-webhook-example.banzaicloud.com/validate"
	admissionWebhookAnnotationStatusKey   = "admission-webhook-example.banzaicloud.com/status"

	etcdEndpoints        = "etcd:2379"
	etcdPodPrefix        = "etcd" /*use this rule to check if a pod is etcd pod */
)

type WebhookServer struct {
	server *http.Server
}

// Webhook Server parameters
type WhSvrParameters struct {
	port           int    // webhook server port
	certFile       string // path to the x509 certificate for https
	keyFile        string // path to the x509 private key matching `CertFile`
}

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = admissionregistrationv1beta1.AddToScheme(runtimeScheme)
	// defaulting with webhooks:
	// https://github.com/kubernetes/kubernetes/issues/57982
	_ = v1.AddToScheme(runtimeScheme)
}

func HasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[0:len(prefix)] == prefix
}

// validate deployments and services
func (whsvr *WebhookServer) validate(ar *v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	req := ar.Request
	var (
	    result    *metav1.Status
		allowed   bool
		deletePodName string
		deletePodId uint64
		memberlistNum int
	)
    //default forbident deleting any pod
	allowed = false
	glog.Infof("AdmissionReview for Kind=%v, Namespace=%v Name=%v (%v) UID=%v patchOperation=%v UserInfo=%v",
		req.Kind, req.Namespace, req.Name, req.UID, req.Operation, req.UserInfo)

	switch req.Kind.Kind {
	case "Pod":
	        pod := corev1.Pod{}
		deserializer := codecs.UniversalDeserializer()
		if _, _, err := deserializer.Decode(ar.Request.Object.Raw, nil, &pod); err != nil {
			glog.Errorf("Could not Decode raw object: %v", err)
			return &v1beta1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
				},
			}
		}

		deletePodName = req.Name
		if false == HasPrefix(deletePodName, etcdPodPrefix){
			allowed = true
			glog.Info("not etcd pod,allow to delete")
			result = &metav1.Status{
				Reason: "not etcd pod,allow to delete",
			}
			goto EXIT
		}

		cli, err := clientv3.New(clientv3.Config{
			Endpoints:   []string{etcdEndpoints},
			DialTimeout: 5 * time.Second,
		})
		if err != nil {
			glog.Errorf("etcd cientv3 New failed,etcd may not started or etcd endpoint is wrong: %v", err)
			result = &metav1.Status{
				Reason: "etcd cientv3 New failed,etcd may not started or etcd endpoint is wrong",
			}
			goto EXIT
		}
		defer cli.Close()
		mapi := clientv3.NewMaintenance(cli)
		status, err := mapi.Status(context.Background(), etcdEndpoints)
		if err != nil {
			glog.Errorf("get etcd cientv3 status failed: %v", err)
			result = &metav1.Status{
				Reason: "get etcd cientv3 status failed",
			}
			goto EXIT
		}

		leaderId := status.Leader
		memberlist, err1 := cli.MemberList(context.Background())
		if err1 != nil {
			glog.Errorf("get memberlist failed: %v", err1)
			result = &metav1.Status{
				Reason: "get memberlist failed",
			}
			goto EXIT
		}

		memberlistNum = len(memberlist.Members)
		for _,v := range memberlist.Members {
			if v.Name == deletePodName {
				deletePodId = v.ID
				break
			}
		}


		glog.Infof("leaderId is: %v", leaderId)
		glog.Infof("deletePodId: %v", deletePodId)
		glog.Infof("status: %v", status)
		glog.Infof("memberlist.Members is: %v", memberlist.Members)

		glog.Infof("req is: %v", req)
		glog.Infof("raw is: %v", req.Object.Raw)
		glog.Infof("pod is: %+v", pod)

		if deletePodId == 0 {
			result = &metav1.Status{
				Reason: "get deletePodId error",
			}
			goto EXIT
		}
		//delete pod when it is not leader or there is just one member
		if deletePodId != leaderId || memberlistNum == 1  {
			allowed = true
		}else {
			allowed = false
			result = &metav1.Status{
				Reason: "delete pod forbidented!",
			}
		}
	}

	EXIT:
	return &v1beta1.AdmissionResponse{
		Allowed: allowed,
		Result:  result,
	}
}

// Serve method for webhook server
func (whsvr *WebhookServer) serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		glog.Error("empty body")
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		glog.Errorf("Content-Type=%s, expect application/json", contentType)
		http.Error(w, "invalid Content-Type, expect `application/json`", http.StatusUnsupportedMediaType)
		return
	}

	var admissionResponse *v1beta1.AdmissionResponse
	ar := v1beta1.AdmissionReview{}
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		glog.Errorf("Can't decode body: %v", err)
		admissionResponse = &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	} else {
		fmt.Println(r.URL.Path)
		if r.URL.Path == "/validate" {
			admissionResponse = whsvr.validate(&ar)
		}
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
		glog.Errorf("Can't encode response: %v", err)
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}
	glog.Infof("Ready to write reponse ...")
	if _, err := w.Write(resp); err != nil {
		glog.Errorf("Can't write response: %v", err)
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}
}

