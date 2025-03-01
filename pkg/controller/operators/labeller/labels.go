package labeller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/sirupsen/logrus"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	operatorsv1 "github.com/operator-framework/api/pkg/operators/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	operatorsv1alpha2 "github.com/operator-framework/api/pkg/operators/v1alpha2"

	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/install"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/operators/decorators"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/ownerutil"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/queueinformer"
)

type ApplyConfig[T any] interface {
	WithLabels(map[string]string) T
}

type Client[A ApplyConfig[A], T metav1.Object] interface {
	Apply(ctx context.Context, cfg ApplyConfig[A], opts metav1.ApplyOptions) (result T, err error)
}

func hasLabel(obj metav1.Object) bool {
	value, ok := obj.GetLabels()[install.OLMManagedLabelKey]
	return ok && value == install.OLMManagedLabelValue
}

func ObjectLabeler[T metav1.Object, A ApplyConfig[A]](
	ctx context.Context,
	logger *logrus.Logger,
	check func(metav1.Object) bool,
	list func(options labels.Selector) ([]T, error),
	applyConfigFor func(name, namespace string) A,
	apply func(namespace string, ctx context.Context, cfg A, opts metav1.ApplyOptions) (T, error),
) func(done func() bool) queueinformer.LegacySyncHandler {
	return func(done func() bool) queueinformer.LegacySyncHandler {
		return func(obj interface{}) error {
			cast, ok := obj.(T)
			if !ok {
				err := fmt.Errorf("wrong type %T, expected %T: %#v", obj, new(T), obj)
				logger.WithError(err).Error("casting failed")
				return fmt.Errorf("casting failed: %w", err)
			}

			if !check(cast) || hasLabel(cast) {
				// if the object we're processing does not need us to label it, it's possible that every object that requires
				// the label already has it; in which case we should exit the process, so the Pod that succeeds us can filter
				// the informers used to drive the controller and stop having to track extraneous objects
				items, err := list(labels.Everything())
				if err != nil {
					logger.WithError(err).Warn("failed to list all objects to check for labelling completion")
					return nil
				}
				gvrFullyLabelled := true
				for _, item := range items {
					gvrFullyLabelled = gvrFullyLabelled && (!check(item) || hasLabel(item))
				}
				if gvrFullyLabelled {
					allObjectsLabelled := done()
					if allObjectsLabelled {
						logrus.Info("detected that every object is labelled, exiting to re-start the process...")
						os.Exit(0)
					}
				}
				return nil
			}

			logger.WithFields(logrus.Fields{"namespace": cast.GetNamespace(), "name": cast.GetName()}).Info("applying ownership label")
			cfg := applyConfigFor(cast.GetName(), cast.GetNamespace())
			cfg.WithLabels(map[string]string{
				install.OLMManagedLabelKey: install.OLMManagedLabelValue,
			})

			_, err := apply(cast.GetNamespace(), ctx, cfg, metav1.ApplyOptions{FieldManager: "olm-ownership-labeller"})
			return err
		}
	}
}

// CRDs did not have applyconfigurations generated for them on accident, we can remove this when
// https://github.com/kubernetes/kubernetes/pull/120177 lands
func ObjectPatchLabeler(
	ctx context.Context,
	logger *logrus.Logger,
	check func(metav1.Object) bool,
	list func(selector labels.Selector) (ret []*metav1.PartialObjectMetadata, err error),
	patch func(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (result *apiextensionsv1.CustomResourceDefinition, err error),
) func(done func() bool) queueinformer.LegacySyncHandler {
	return func(done func() bool) queueinformer.LegacySyncHandler {
		return func(obj interface{}) error {
			cast, ok := obj.(*metav1.PartialObjectMetadata)
			if !ok {
				err := fmt.Errorf("wrong type %T, expected %T: %#v", obj, new(*metav1.PartialObjectMetadata), obj)
				logger.WithError(err).Error("casting failed")
				return fmt.Errorf("casting failed: %w", err)
			}

			if !check(cast) || hasLabel(cast) {
				// if the object we're processing does not need us to label it, it's possible that every object that requires
				// the label already has it; in which case we should exit the process, so the Pod that succeeds us can filter
				// the informers used to drive the controller and stop having to track extraneous objects
				items, err := list(labels.Everything())
				if err != nil {
					logger.WithError(err).Warn("failed to list all objects to check for labelling completion")
					return nil
				}
				gvrFullyLabelled := true
				for _, item := range items {
					gvrFullyLabelled = gvrFullyLabelled && (!check(item) || hasLabel(item))
				}
				if gvrFullyLabelled {
					allObjectsLabelled := done()
					if allObjectsLabelled {
						logrus.Info("detected that every object is labelled, exiting to re-start the process...")
						os.Exit(0)
					}
				}
				return nil
			}

			uid := cast.GetUID()
			rv := cast.GetResourceVersion()

			// to ensure they appear in the patch as preconditions
			previous := cast.DeepCopy()
			previous.SetUID("")
			previous.SetResourceVersion("")

			oldData, err := json.Marshal(previous)
			if err != nil {
				return fmt.Errorf("failed to Marshal old data for %s/%s: %w", previous.GetNamespace(), previous.GetName(), err)
			}

			// to ensure they appear in the patch as preconditions
			updated := cast.DeepCopy()
			updated.SetUID(uid)
			updated.SetResourceVersion(rv)
			labels := updated.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			labels[install.OLMManagedLabelKey] = install.OLMManagedLabelValue
			updated.SetLabels(labels)

			newData, err := json.Marshal(updated)
			if err != nil {
				return fmt.Errorf("failed to Marshal old data for %s/%s: %w", updated.GetNamespace(), updated.GetName(), err)
			}

			logger.WithFields(logrus.Fields{"namespace": cast.GetNamespace(), "name": cast.GetName()}).Info("patching ownership label")
			patchBytes, err := jsonpatch.CreateMergePatch(oldData, newData)
			if err != nil {
				return fmt.Errorf("failed to create patch for %s/%s: %w", cast.GetNamespace(), cast.GetName(), err)
			}

			_, err = patch(ctx, cast.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{FieldManager: "olm-ownership-labeller"})
			return err
		}
	}
}

// HasOLMOwnerRef determines if an object is owned by another object in the OLM Groups.
// This checks both classical OwnerRefs and the "OLM OwnerRef" in labels to handle
// cluster-scoped resources.
func HasOLMOwnerRef(object metav1.Object) bool {
	for _, ref := range object.GetOwnerReferences() {
		for _, gv := range []schema.GroupVersion{
			operatorsv1.GroupVersion,
			operatorsv1alpha1.SchemeGroupVersion,
			operatorsv1alpha2.GroupVersion,
		} {
			if ref.APIVersion == gv.String() {
				return true
			}
		}
	}
	hasOLMOwnerLabels := true
	for _, label := range []string{ownerutil.OwnerKey, ownerutil.OwnerNamespaceKey, ownerutil.OwnerKind} {
		_, exists := object.GetLabels()[label]
		hasOLMOwnerLabels = hasOLMOwnerLabels && exists
	}
	return hasOLMOwnerLabels
}

func HasOLMLabel(object metav1.Object) bool {
	for key := range object.GetLabels() {
		if strings.HasPrefix(key, decorators.ComponentLabelKeyPrefix) {
			return true
		}
	}
	return false
}
