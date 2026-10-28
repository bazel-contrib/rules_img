// Package aws is a minimal, purpose-built replacement for the parts of the AWS
// SDK for Go v2 that the img tool needs: resolving AWS credentials in process
// and signing requests with Signature Version 4.
//
// It deliberately covers only what is required to authenticate to Amazon ECR
// and to presign / probe S3 objects. Credential resolution follows the same
// sources and precedence as the AWS SDKs and CLI:
//
//   - AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN
//   - the shared config and credentials files (AWS_PROFILE, AWS_CONFIG_FILE,
//     AWS_SHARED_CREDENTIALS_FILE), including static keys, credential_process,
//     role_arn with source_profile / credential_source (STS AssumeRole),
//     web_identity_token_file (STS AssumeRoleWithWebIdentity) and IAM Identity
//     Center (sso_session / sso_start_url)
//   - AWS_WEB_IDENTITY_TOKEN_FILE + AWS_ROLE_ARN (for example EKS IRSA)
//   - the container credentials endpoint (AWS_CONTAINER_CREDENTIALS_*, for
//     example ECS and EKS Pod Identity)
//   - the EC2 instance metadata service (IMDSv2, falling back to IMDSv1)
//
// Not supported: `aws login` console sessions (login_session), MFA prompts
// (mfa_serial) and the on-disk caching of assumed-role credentials.
package aws
