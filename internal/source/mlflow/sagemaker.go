package mlflow

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/abhishek-rnjn/evals.si/internal/source"
)

var regionName = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)

// sagemaker sets the connector up for a SageMaker managed MLflow tracking
// server or app. Both speak the open-source MLflow API behind AWS Signature
// Version 4; the server is chosen by a header carrying its ARN. The service
// name, the header and the host come from the sagemaker-mlflow client
// (v0.5.0: auth_provider.py, mlflow_sagemaker_request_header_provider.py and
// mlflow_sagemaker_helpers.py).
func (c *connector) sagemaker(u *url.URL, token string) error {
	auth := c.src.GetAuth().GetSigv4()
	arn := auth.GetArn()
	// arn:<partition>:sagemaker:<region>:<account>:<resource type>/<name>
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "sagemaker" || !regionName.MatchString(parts[3]) {
		return fmt.Errorf("mlflow sagemaker: auth.sigv4.arn must be the ARN of an MLflow tracking server or app, like arn:aws:sagemaker:us-east-1:123456789012:mlflow-tracking-server/studio")
	}
	region := parts[3]
	resourceType, name, _ := strings.Cut(parts[5], "/")
	var service, header string
	switch resourceType {
	case "mlflow-tracking-server":
		service, header = "sagemaker-mlflow", "x-mlflow-sm-tracking-server-arn"
	case "mlflow-app":
		service, header = "sagemaker", "x-sm-mlflow-app-arn"
	default:
		return fmt.Errorf("mlflow sagemaker: auth.sigv4.arn names a %q; only mlflow-tracking-server and mlflow-app are MLflow stores", resourceType)
	}
	if name == "" || parts[4] == "" {
		return fmt.Errorf("mlflow sagemaker: auth.sigv4.arn has no account or resource name")
	}
	if auth.GetRegion() != "" && auth.GetRegion() != region {
		return fmt.Errorf("mlflow sagemaker: auth.sigv4.region %q is not the ARN's region %q; leave it empty", auth.GetRegion(), region)
	}
	if auth.GetService() != "" && auth.GetService() != service {
		return fmt.Errorf("mlflow sagemaker: auth.sigv4.service %q is not %q, which a %s signs with; leave it empty", auth.GetService(), service, resourceType)
	}
	// A request is signed with the source's AWS credential, so it goes only to
	// an AWS host: the signing must not hand a signature to a lookalike.
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" || !(strings.HasSuffix(host, ".sagemaker.aws") || strings.HasSuffix(host, ".app.aws") ||
		strings.HasSuffix(host, ".amazonaws.com") || strings.HasSuffix(host, ".amazonaws.com.cn")) {
		return fmt.Errorf("mlflow sagemaker: endpoint must be an https AWS host, like https://%s.experiments.sagemaker.aws, got %q", region, c.src.GetEndpoint())
	}
	signer := &sigV4{region: region, service: service, now: time.Now}
	if token != source.ValidationToken {
		creds, err := parseAWSCredentials(token)
		if err != nil {
			return err
		}
		signer.creds = creds
	}
	c.auth = signer
	c.headers = map[string]string{header: arn}
	return nil
}
