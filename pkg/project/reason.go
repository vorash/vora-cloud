package project

import (
	"errors"
	"fmt"

	"github.com/vorash/vora-cloud/pkg/deploy"
)

// A cluster's own error text can name node addresses, service hosts and
// namespaces. A deployment's Error is shown by the dashboard, REST and gRPC,
// so the service stores one of the reasons below and logs the raw text with
// the deployment id for the operator.

const (
	reasonUnreachable = "The region's cluster could not be reached. Try again in a moment."
	reasonRefused     = "The cluster refused the deployment."
	reasonTimedOut    = "The app did not become ready in time. Check that it listens on the project's port and answers its health path."
)

// deployFailure is the reason shown when applying a deployment to the cluster
// failed with err.
func deployFailure(d *Deployment, err error) string {
	switch {
	case errors.Is(err, deploy.ErrUnreachable):
		return reasonUnreachable
	case errors.Is(err, deploy.ErrRefused), errors.Is(err, deploy.ErrInvalidSpec):
		return reasonRefused
	}
	return genericFailure(d)
}

// rolloutFailure is the reason shown when the cluster reports a failed rollout.
func rolloutFailure(d *Deployment, st deploy.Status) string {
	switch st.Reason {
	case "ProgressDeadlineExceeded":
		return reasonTimedOut
	case "ImagePullBackOff", "ErrImagePull":
		// The image name is the user's own, so it can be shown.
		return fmt.Sprintf("The image %q could not be pulled. Check that it exists and that the region can reach its registry.", d.Image)
	}
	return genericFailure(d)
}

func genericFailure(d *Deployment) string {
	return fmt.Sprintf("The deployment failed. Quote deployment %s if you ask for help.", d.ID)
}
