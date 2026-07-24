/*
Copyright 2026.

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

package fscluster

import fsv1alpha1 "github.com/go-faster/fs-operator/api/v1alpha1"

// MinSecretKeyLength is fs's minimum S3 secret-key length. A shorter secret is
// refused rather than pushed to the cluster: the FSAccessKey controller reports
// it on the key's status (ReasonWeakSecretKey).
const MinSecretKeyLength = 16

// EndpointKey holds the cluster's S3 endpoint in a generated credential Secret,
// alongside AccessKeyKey and SecretKeyKey, so the Secret is enough to configure
// a client.
const EndpointKey = "endpoint"

// AccessKeySecretName is the Secret an FSAccessKey's generated credential is
// written to: the user-named one, or <name>-credentials by default.
func AccessKeySecretName(key *fsv1alpha1.FSAccessKey) string {
	if key.Spec.SecretName != "" {
		return key.Spec.SecretName
	}

	return key.Name + "-credentials"
}
