package strategyrestrictions

import (
	"fmt"
	"io"
	"strings"

	"github.com/openshift/origin/pkg/build/buildscheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	kapihelper "k8s.io/kubernetes/pkg/apis/core/helper"
	"k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	authorizationclient "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset/typed/authorization/internalversion"
	kubeadmission "k8s.io/kubernetes/pkg/kubeapiserver/admission"
	rbacregistry "k8s.io/kubernetes/pkg/registry/rbac"

	buildclient "github.com/openshift/client-go/build/clientset/versioned"
	"github.com/openshift/origin/pkg/authorization/util"
	buildapi "github.com/openshift/origin/pkg/build/apis/build"
	oadmission "github.com/openshift/origin/pkg/cmd/server/admission"
	"github.com/openshift/origin/pkg/cmd/server/bootstrappolicy"
	"k8s.io/kubernetes/pkg/apis/authorization"
)

func Register(plugins *admission.Plugins) {
	plugins.Register("BuildByStrategy",
		func(config io.Reader) (admission.Interface, error) {
			return NewBuildByStrategy(), nil
		})
}

type buildByStrategy struct {
	*admission.Handler
	sarClient   authorizationclient.SubjectAccessReviewInterface
	buildClient buildclient.Interface
}

var _ = kubeadmission.WantsInternalKubeClientSet(&buildByStrategy{})
var _ = oadmission.WantsOpenshiftInternalBuildClient(&buildByStrategy{})

// NewBuildByStrategy returns an admission control for builds that checks
// on policy based on the build strategy type
func NewBuildByStrategy() admission.Interface {
	return &buildByStrategy{
		Handler: admission.NewHandler(admission.Create, admission.Update),
	}
}

func (a *buildByStrategy) Admit(attr admission.Attributes) error {
	gr := attr.GetResource().GroupResource()
	switch gr {
	case buildapi.Resource("buildconfigs"),
		buildapi.LegacyResource("buildconfigs"):
	case buildapi.Resource("builds"),
		buildapi.LegacyResource("builds"):
		// Explicitly exclude the builds/details subresource because it's only
		// updating commit info and cannot change build type.
		if attr.GetSubresource() == "details" {
			return nil
		}
	default:
		return nil
	}

	// if this is an update, see if we are only updating the ownerRef.  Garbage collection does this
	// and we should allow it in general, since you had the power to update and the power to delete.
	// The worst that happens is that you delete something, but you aren't controlling the privileged object itself
	if attr.GetOldObject() != nil && rbacregistry.IsOnlyMutatingGCFields(attr.GetObject(), attr.GetOldObject(), kapihelper.Semantic) {
		return nil
	}

	switch obj := attr.GetObject().(type) {
	case *buildapi.Build:
		return a.checkBuildAuthorization(obj, attr)
	case *buildapi.BuildConfig:
		return a.checkBuildConfigAuthorization(obj, attr)
	case *buildapi.BuildRequest:
		return a.checkBuildRequestAuthorization(obj, attr)
	default:
		return admission.NewForbidden(attr, fmt.Errorf("unrecognized request object %#v", obj))
	}
}

func (a *buildByStrategy) SetInternalKubeClientSet(c internalclientset.Interface) {
	a.sarClient = c.Authorization().SubjectAccessReviews()
}

func (a *buildByStrategy) SetOpenshiftInternalBuildClient(c buildclient.Interface) {
	a.buildClient = c
}

func (a *buildByStrategy) ValidateInitialization() error {
	if a.buildClient == nil {
		return fmt.Errorf("BuildByStrategy needs an Openshift buildClient")
	}
	if a.sarClient == nil {
		return fmt.Errorf("BuildByStrategy needs an Openshift sarClient")
	}
	return nil
}

func resourceForStrategyType(strategy buildapi.BuildStrategy) (schema.GroupResource, error) {
	switch {
	case strategy.DockerStrategy != nil && strategy.DockerStrategy.ImageOptimizationPolicy != nil && *strategy.DockerStrategy.ImageOptimizationPolicy != buildapi.ImageOptimizationNone:
		return buildapi.Resource(bootstrappolicy.OptimizedDockerBuildResource), nil
	case strategy.DockerStrategy != nil:
		return buildapi.Resource(bootstrappolicy.DockerBuildResource), nil
	case strategy.CustomStrategy != nil:
		return buildapi.Resource(bootstrappolicy.CustomBuildResource), nil
	case strategy.SourceStrategy != nil:
		return buildapi.Resource(bootstrappolicy.SourceBuildResource), nil
	case strategy.JenkinsPipelineStrategy != nil:
		return buildapi.Resource(bootstrappolicy.JenkinsPipelineBuildResource), nil
	default:
		return schema.GroupResource{}, fmt.Errorf("unrecognized build strategy: %#v", strategy)
	}
}

func resourceName(objectMeta metav1.ObjectMeta) string {
	if len(objectMeta.GenerateName) > 0 {
		return objectMeta.GenerateName
	}
	return objectMeta.Name
}

func (a *buildByStrategy) checkBuildAuthorization(build *buildapi.Build, attr admission.Attributes) error {
	strategy := build.Spec.Strategy
	resource, err := resourceForStrategyType(strategy)
	if err != nil {
		return admission.NewForbidden(attr, err)
	}
	subresource := ""
	tokens := strings.SplitN(resource.Resource, "/", 2)
	resourceType := tokens[0]
	if len(tokens) == 2 {
		subresource = tokens[1]
	}

	sar := util.AddUserToSAR(attr.GetUserInfo(), &authorization.SubjectAccessReview{
		Spec: authorization.SubjectAccessReviewSpec{
			ResourceAttributes: &authorization.ResourceAttributes{
				Namespace:   attr.GetNamespace(),
				Verb:        "create",
				Group:       resource.Group,
				Resource:    resourceType,
				Subresource: subresource,
				Name:        resourceName(build.ObjectMeta),
			},
		},
	})
	return a.checkAccess(strategy, sar, attr)
}

func (a *buildByStrategy) checkBuildConfigAuthorization(buildConfig *buildapi.BuildConfig, attr admission.Attributes) error {
	strategy := buildConfig.Spec.Strategy
	resource, err := resourceForStrategyType(strategy)
	if err != nil {
		return admission.NewForbidden(attr, err)
	}
	subresource := ""
	tokens := strings.SplitN(resource.Resource, "/", 2)
	resourceType := tokens[0]
	if len(tokens) == 2 {
		subresource = tokens[1]
	}

	sar := util.AddUserToSAR(attr.GetUserInfo(), &authorization.SubjectAccessReview{
		Spec: authorization.SubjectAccessReviewSpec{
			ResourceAttributes: &authorization.ResourceAttributes{
				Namespace:   attr.GetNamespace(),
				Verb:        "create",
				Group:       resource.Group,
				Resource:    resourceType,
				Subresource: subresource,
				Name:        resourceName(buildConfig.ObjectMeta),
			},
		},
	})
	return a.checkAccess(strategy, sar, attr)
}

func (a *buildByStrategy) checkBuildRequestAuthorization(req *buildapi.BuildRequest, attr admission.Attributes) error {
	gr := attr.GetResource().GroupResource()
	switch gr {
	case buildapi.Resource("builds"),
		buildapi.LegacyResource("builds"):
		build, err := a.buildClient.BuildV1().Builds(attr.GetNamespace()).Get(req.Name, metav1.GetOptions{})
		if err != nil {
			return admission.NewForbidden(attr, err)
		}
		internalBuild := &buildapi.Build{}
		if err := buildscheme.InternalExternalScheme.Convert(build, internalBuild, nil); err != nil {
			return admission.NewForbidden(attr, err)
		}
		return a.checkBuildAuthorization(internalBuild, attr)

	case buildapi.Resource("buildconfigs"),
		buildapi.LegacyResource("buildconfigs"):
		buildConfig, err := a.buildClient.BuildV1().BuildConfigs(attr.GetNamespace()).Get(req.Name, metav1.GetOptions{})
		if err != nil {
			return admission.NewForbidden(attr, err)
		}
		internalBuildConfig := &buildapi.BuildConfig{}
		if err := buildscheme.InternalExternalScheme.Convert(buildConfig, internalBuildConfig, nil); err != nil {
			return admission.NewForbidden(attr, err)
		}
		return a.checkBuildConfigAuthorization(internalBuildConfig, attr)
	default:
		return admission.NewForbidden(attr, fmt.Errorf("Unknown resource type %s for BuildRequest", attr.GetResource()))
	}
}

func (a *buildByStrategy) checkAccess(strategy buildapi.BuildStrategy, subjectAccessReview *authorization.SubjectAccessReview, attr admission.Attributes) error {
	resp, err := a.sarClient.Create(subjectAccessReview)
	if err != nil {
		return admission.NewForbidden(attr, err)
	}
	if !resp.Status.Allowed {
		return notAllowed(strategy, attr)
	}
	return nil
}

func notAllowed(strategy buildapi.BuildStrategy, attr admission.Attributes) error {
	return admission.NewForbidden(attr, fmt.Errorf("build strategy %s is not allowed", strategyTypeString(strategy)))
}

func strategyTypeString(strategy buildapi.BuildStrategy) string {
	switch {
	case strategy.DockerStrategy != nil:
		return "Docker"
	case strategy.CustomStrategy != nil:
		return "Custom"
	case strategy.SourceStrategy != nil:
		return "Source"
	case strategy.JenkinsPipelineStrategy != nil:
		return "JenkinsPipeline"
	}
	return ""
}
