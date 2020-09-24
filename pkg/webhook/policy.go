/*

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

package webhook

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	opa "github.com/open-policy-agent/frameworks/constraint/pkg/client"
	"github.com/open-policy-agent/frameworks/constraint/pkg/core/templates"
	rtypes "github.com/open-policy-agent/frameworks/constraint/pkg/types"
	"github.com/open-policy-agent/gatekeeper/apis"
	"github.com/open-policy-agent/gatekeeper/apis/config/v1alpha1"
	"github.com/open-policy-agent/gatekeeper/pkg/controller/config/process"
	"github.com/open-policy-agent/gatekeeper/pkg/keys"
	"github.com/open-policy-agent/gatekeeper/pkg/target"
	"github.com/open-policy-agent/gatekeeper/pkg/util"
	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	clientcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func init() {
	AddToManagerFuncs = append(AddToManagerFuncs, AddPolicyWebhook)
	if err := apis.AddToScheme(runtimeScheme); err != nil {
		log.Error(err, "unable to add to scheme")
		panic(err)
	}
}

var log = logf.Log.WithName("webhook")

const (
	serviceAccountName = "gatekeeper-admin"
)

var (
	runtimeScheme                      = k8sruntime.NewScheme()
	codecs                             = serializer.NewCodecFactory(runtimeScheme)
	deserializer                       = codecs.UniversalDeserializer()
	disableEnforcementActionValidation = flag.Bool("disable-enforcementaction-validation", false, "disable validation of the enforcementAction field of a constraint")
	logDenies                          = flag.Bool("log-denies", false, "log detailed info on each deny")
	emitAdmissionEvents                = flag.Bool("emit-admission-events", false, "(alpha) emit Kubernetes events in gatekeeper namespace for each admission violation")
	serviceaccount                     = fmt.Sprintf("system:serviceaccount:%s:%s", util.GetNamespace(), serviceAccountName)
	// webhookName is deprecated, set this on the manifest YAML if needed"
)

// +kubebuilder:webhook:verbs=create;update,path=/v1/admit,mutating=false,failurePolicy=ignore,groups=*,resources=*,versions=*,name=validation.gatekeeper.sh
// +kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch

// AddPolicyWebhook registers the policy webhook server with the manager
func AddPolicyWebhook(mgr manager.Manager, opa *opa.Client, processExcluder *process.Excluder) error {
	reporter, err := newStatsReporter()
	if err != nil {
		return err
	}
	eventBroadcaster := record.NewBroadcaster()
	kubeClient := kubernetes.NewForConfigOrDie(mgr.GetConfig())
	eventBroadcaster.StartRecordingToSink(&clientcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(
		scheme.Scheme,
		corev1.EventSource{Component: "gatekeeper-webhook"})
	wh := &admission.Webhook{
		Handler: &validationHandler{
			opa:             opa,
			client:          mgr.GetClient(),
			reader:          mgr.GetAPIReader(),
			reporter:        reporter,
			processExcluder: processExcluder,
			eventRecorder:   recorder,
			gkNamespace:     util.GetNamespace(),
		},
	}
	// TODO(https://github.com/open-policy-agent/gatekeeper/issues/661): remove log injection if the race condition in the cited bug is eliminated.
	// Otherwise we risk having unstable logger names for the webhook.
	if err := wh.InjectLogger(log); err != nil {
		return err
	}
	mgr.GetWebhookServer().Register("/v1/admit", wh)
	return nil
}

var _ admission.Handler = &validationHandler{}

type validationHandler struct {
	opa      *opa.Client
	client   client.Client
	reporter StatsReporter
	// reader that will be configured to use the API server
	// obtained from mgr.GetAPIReader()
	reader client.Reader
	// for testing
	injectedConfig  *v1alpha1.Config
	processExcluder *process.Excluder
	eventRecorder   record.EventRecorder
	gkNamespace     string
}

type requestResponse string

const (
	errorResponse   requestResponse = "error"
	denyResponse    requestResponse = "deny"
	allowResponse   requestResponse = "allow"
	unknownResponse requestResponse = "unknown"
)

// Handle the validation request
func (h *validationHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := log.WithValues("hookType", "validation")

	var timeStart = time.Now()

	if isGkServiceAccount(req.AdmissionRequest.UserInfo) {
		return admission.ValidationResponse(true, "Gatekeeper does not self-manage")
	}

	if req.AdmissionRequest.Operation == admissionv1beta1.Delete {
		// oldObject is the existing object.
		// It is null for DELETE operations in API servers prior to v1.15.0.
		// https://github.com/kubernetes/website/pull/14671
		if req.AdmissionRequest.OldObject.Raw == nil {
			vResp := admission.ValidationResponse(false, "For admission webhooks registered for DELETE operations, please use Kubernetes v1.15.0+.")
			vResp.Result.Code = http.StatusInternalServerError
			return vResp
		}
		// For admission webhooks registered for DELETE operations on k8s built APIs or CRDs,
		// the apiserver now sends the existing object as admissionRequest.Request.OldObject to the webhook
		// object is the new object being admitted.
		// It is null for DELETE operations.
		// https://github.com/kubernetes/kubernetes/pull/76346
		req.AdmissionRequest.Object = req.AdmissionRequest.OldObject
	}

	if userErr, err := h.validateGatekeeperResources(ctx, req); err != nil {
		vResp := admission.ValidationResponse(false, err.Error())
		if vResp.Result == nil {
			vResp.Result = &metav1.Status{}
		}
		if userErr {
			vResp.Result.Code = http.StatusUnprocessableEntity
		} else {
			vResp.Result.Code = http.StatusInternalServerError
		}
		return vResp
	}

	requestResponse := unknownResponse
	defer func() {
		if h.reporter != nil {
			if err := h.reporter.ReportRequest(
				requestResponse, time.Since(timeStart)); err != nil {
				log.Error(err, "failed to report request")
			}
		}
	}()

	// namespace is excluded from webhook using config
	if h.skipExcludedNamespace(req.AdmissionRequest.Namespace) {
		requestResponse = allowResponse
		return admission.ValidationResponse(true, "Namespace is set to be ignored by Gatekeeper config")
	}

	resp, err := h.reviewRequest(ctx, req)
	if err != nil {
		log.Error(err, "error executing query")
		vResp := admission.ValidationResponse(false, err.Error())
		if vResp.Result == nil {
			vResp.Result = &metav1.Status{}
		}
		vResp.Result.Code = http.StatusInternalServerError
		requestResponse = errorResponse
		return vResp
	}

	res := resp.Results()
	msgs := h.getDenyMessages(res, req)
	if len(msgs) > 0 {
		vResp := admission.ValidationResponse(false, strings.Join(msgs, "\n"))
		if vResp.Result == nil {
			vResp.Result = &metav1.Status{}
		}
		vResp.Result.Code = http.StatusForbidden
		requestResponse = denyResponse
		return vResp
	}

	requestResponse = allowResponse
	return admission.ValidationResponse(true, "")
}

func (h *validationHandler) getDenyMessages(res []*rtypes.Result, req admission.Request) []string {
	var msgs []string
	var resourceName string
	if len(res) > 0 && (*logDenies || *emitAdmissionEvents) {
		resourceName = req.AdmissionRequest.Name
		if len(resourceName) == 0 && req.AdmissionRequest.Object.Raw != nil {
			// On a CREATE operation, the client may omit name and
			// rely on the server to generate the name.
			obj := &unstructured.Unstructured{}
			if _, _, err := deserializer.Decode(req.AdmissionRequest.Object.Raw, nil, obj); err == nil {
				resourceName = obj.GetName()
			}
		}
	}
	for _, r := range res {
		if r.EnforcementAction == "deny" || r.EnforcementAction == "dryrun" {
			if *logDenies {
				log.WithValues(
					"process", "admission",
					"event_type", "violation",
					"constraint_name", r.Constraint.GetName(),
					"constraint_kind", r.Constraint.GetKind(),
					"constraint_action", r.EnforcementAction,
					"resource_kind", req.AdmissionRequest.Kind.Kind,
					"resource_namespace", req.AdmissionRequest.Namespace,
					"resource_name", resourceName,
					"request_username", req.AdmissionRequest.UserInfo.Username,
				).Info("denied admission")
			}
			if *emitAdmissionEvents {
				annotations := map[string]string{
					"process":            "admission",
					"event_type":         "violation",
					"constraint_name":    r.Constraint.GetName(),
					"constraint_kind":    r.Constraint.GetKind(),
					"constraint_action":  r.EnforcementAction,
					"resource_kind":      req.AdmissionRequest.Kind.Kind,
					"resource_namespace": req.AdmissionRequest.Namespace,
					"resource_name":      resourceName,
					"request_username":   req.AdmissionRequest.UserInfo.Username,
				}
				eventMsg := "Admission webhook \"validation.gatekeeper.sh\" denied request"
				reason := "FailedAdmission"
				if r.EnforcementAction == "dryrun" {
					eventMsg = "Dryrun violation"
					reason = "DryrunViolation"
				}
				ref := getViolationRef(h.gkNamespace, req.AdmissionRequest.Kind.Kind, resourceName, req.AdmissionRequest.Namespace, r.Constraint.GetKind(), r.Constraint.GetName(), r.Constraint.GetNamespace())
				h.eventRecorder.AnnotatedEventf(ref, annotations, corev1.EventTypeWarning, reason, "%s, Resource Namespace: %s, Constraint: %s, Message: %s", eventMsg, req.AdmissionRequest.Namespace, r.Constraint.GetName(), r.Msg)
			}

		}
		// only deny enforcementAction should prompt deny admission response
		if r.EnforcementAction == "deny" {
			msgs = append(msgs, fmt.Sprintf("[denied by %s] %s", r.Constraint.GetName(), r.Msg))
		}
	}
	return msgs
}

func (h *validationHandler) getConfig(ctx context.Context) (*v1alpha1.Config, error) {
	if h.injectedConfig != nil {
		return h.injectedConfig, nil
	}
	if h.client == nil {
		return nil, errors.New("no client available to retrieve validation config")
	}
	cfg := &v1alpha1.Config{}
	return cfg, h.client.Get(ctx, keys.Config, cfg)
}

func isGkServiceAccount(user authenticationv1.UserInfo) bool {
	return user.Username == serviceaccount
}

// validateGatekeeperResources returns whether an issue is user error (vs internal) and any errors
// validating internal resources
func (h *validationHandler) validateGatekeeperResources(ctx context.Context, req admission.Request) (bool, error) {
	if req.AdmissionRequest.Kind.Group == "templates.gatekeeper.sh" && req.AdmissionRequest.Kind.Kind == "ConstraintTemplate" {
		return h.validateTemplate(ctx, req)
	}
	if req.AdmissionRequest.Kind.Group == "constraints.gatekeeper.sh" {
		return h.validateConstraint(ctx, req)
	}
	return false, nil
}

func (h *validationHandler) validateTemplate(ctx context.Context, req admission.Request) (bool, error) {
	templ, _, err := deserializer.Decode(req.AdmissionRequest.Object.Raw, nil, nil)
	if err != nil {
		return false, err
	}
	unversioned := &templates.ConstraintTemplate{}
	if err := runtimeScheme.Convert(templ, unversioned, nil); err != nil {
		return false, err
	}
	if _, err := h.opa.CreateCRD(ctx, unversioned); err != nil {
		return true, err
	}
	return false, nil
}

func (h *validationHandler) validateConstraint(ctx context.Context, req admission.Request) (bool, error) {
	obj := &unstructured.Unstructured{}
	if _, _, err := deserializer.Decode(req.AdmissionRequest.Object.Raw, nil, obj); err != nil {
		return false, err
	}
	if err := h.opa.ValidateConstraint(ctx, obj); err != nil {
		return true, err
	}

	enforcementActionString, found, err := unstructured.NestedString(obj.Object, "spec", "enforcementAction")
	if err != nil {
		return false, err
	}
	enforcementAction := util.EnforcementAction(enforcementActionString)
	if found && enforcementAction != "" {
		if !*disableEnforcementActionValidation {
			err = util.ValidateEnforcementAction(enforcementAction)
			if err != nil {
				return false, err
			}
		}
	} else {
		return true, nil
	}
	return false, nil
}

// traceSwitch returns true if a request should be traced
func (h *validationHandler) reviewRequest(ctx context.Context, req admission.Request) (*rtypes.Responses, error) {
	trace, dump := h.tracingLevel(ctx, req)
	// Coerce server-side apply admission requests into treating namespaces
	// the same way as older admission requests. See
	// https://github.com/open-policy-agent/gatekeeper/issues/792
	if req.Kind.Kind == "Namespace" && req.Kind.Group == "" {
		req.Namespace = ""
	}
	review := &target.AugmentedReview{AdmissionRequest: &req.AdmissionRequest}
	if req.AdmissionRequest.Namespace != "" {
		ns := &corev1.Namespace{}
		if err := h.client.Get(ctx, types.NamespacedName{Name: req.AdmissionRequest.Namespace}, ns); err != nil {
			if !k8serrors.IsNotFound(err) {
				return nil, err
			}
			// bypass cached client and ask api-server directly
			err = h.reader.Get(ctx, types.NamespacedName{Name: req.AdmissionRequest.Namespace}, ns)
			if err != nil {
				return nil, err
			}
		}
		review.Namespace = ns
	}

	resp, err := h.opa.Review(ctx, review, opa.Tracing(trace))
	if trace {
		log.Info(resp.TraceDump())
	}
	if dump {
		dump, err := h.opa.Dump(ctx)
		if err != nil {
			log.Error(err, "dump error")
		} else {
			log.Info(dump)
		}
	}
	return resp, err
}

func (h *validationHandler) tracingLevel(ctx context.Context, req admission.Request) (bool, bool) {
	cfg, _ := h.getConfig(ctx)
	traceEnabled := false
	dump := false
	for _, trace := range cfg.Spec.Validation.Traces {
		if trace.User != req.AdmissionRequest.UserInfo.Username {
			continue
		}
		gvk := v1alpha1.GVK{
			Group:   req.AdmissionRequest.Kind.Group,
			Version: req.AdmissionRequest.Kind.Version,
			Kind:    req.AdmissionRequest.Kind.Kind,
		}
		if gvk == trace.Kind {
			traceEnabled = true
			if strings.EqualFold(trace.Dump, "All") {
				dump = true
			}
		}
	}
	return traceEnabled, dump
}

func (h *validationHandler) skipExcludedNamespace(namespace string) bool {
	return h.processExcluder.IsNamespaceExcluded(process.Webhook, namespace)
}

func getViolationRef(gkNamespace, rkind, rname, rnamespace, ckind, cname, cnamespace string) *corev1.ObjectReference {
	return &corev1.ObjectReference{
		Kind:      rkind,
		Name:      rname,
		UID:       types.UID(rkind + "/" + rnamespace + "/" + rname + "/" + ckind + "/" + cnamespace + "/" + cname),
		Namespace: gkNamespace,
	}
}
