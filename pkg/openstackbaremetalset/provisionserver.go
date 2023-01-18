package openstackbaremetalset

import (
	"context"

	"github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	baremetalv1 "github.com/openstack-k8s-operators/openstack-baremetal-operator/api/v1beta1"
	"github.com/openstack-k8s-operators/openstack-baremetal-operator/pkg/openstackprovisionserver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func ProvisionServerCreateOrUpdate(
	ctx context.Context,
	helper *helper.Helper,
	instance *baremetalv1.OpenStackBaremetalSet,
) (*baremetalv1.OpenStackProvisionServer, error) {
	l := log.FromContext(ctx)

	// Next deploy the provisioning image (Apache) server
	provisionServer := &baremetalv1.OpenStackProvisionServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.ObjectMeta.Name + "-provisionserver",
			Namespace: instance.ObjectMeta.Namespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, helper.GetClient(), provisionServer, func() error {
		// Assign the prov server its existing port if this is an update, otherwise pick a new one
		// based on what is available
		err := openstackprovisionserver.AssignProvisionServerPort(
			ctx,
			helper,
			provisionServer,
			openstackprovisionserver.DefaultPort,
		)
		if err != nil {
			return err
		}

		provisionServer.Spec.RhelImageURL = instance.Spec.RhelImageURL
		// TODO: Remove hardcoded images below once handled elsewhere
		provisionServer.Spec.AgentImageURL = "quay.io/openstack-k8s-operators/openstack-baremetal-operator-agent:v0.0.1"
		provisionServer.Spec.ApacheImageURL = "registry.redhat.io/rhel8/httpd-24:latest"
		provisionServer.Spec.DownloaderImageURL = "quay.io/openstack-k8s-operators/openstack-baremetal-downloader:v0.0.1"

		err = controllerutil.SetControllerReference(instance, provisionServer, helper.GetScheme())
		if err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return provisionServer, err
	}
	if op != controllerutil.OperationResultNone {
		l.Info("OpenStackProvisionServer %s successfully reconciled - operation: %s", provisionServer.Name, string(op))
	}

	return provisionServer, nil
}
