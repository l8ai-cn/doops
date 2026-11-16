package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	cosBucketName         = "doops-1253657668"
	cosGatewayPutMaxBytes = int64(16 << 20)
	cosControlMaxBytes    = int64(1 << 20)
	cosPresignGetTTL      = 24 * time.Hour
	cosPresignPutTTL      = 15 * time.Minute
	cosDefaultEndpoint    = "https://cos.ap-guangzhou.myqcloud.com"
	cosDefaultRegion      = "ap-guangzhou"
)

const (
	cosErrCredentialMissing        = "cos_credential_missing"
	cosErrNotConfigured            = "cos_not_configured"
	cosErrCredentialInvalid        = "cos_credential_invalid"
	cosErrBucketMismatch           = "cos_bucket_mismatch"
	cosErrBucketUnavailable        = "cos_bucket_unavailable"
	cosErrKeyRejected              = "cos_key_rejected"
	cosErrGatewayPutLimit          = "cos_gateway_put_limit"
	cosErrSizeMismatch             = "cos_size_mismatch"
	cosErrPutFailed                = "cos_put_failed"
	cosErrObjectMissing            = "cos_object_missing"
	cosErrManifestInvalid          = "cos_manifest_invalid"
	cosErrWorkspaceNotSync         = "cos_workspace_not_synced"
	cosErrWorkspaceStale           = "cos_workspace_not_updated"
	cosErrPermissionsMissing       = "cos_snapshot_permissions_missing"
	cosErrAgentSnapshotUnsupported = "cos_agent_snapshot_executable_unsupported"
)

type cosFailure struct {
	category string
}

func (e *cosFailure) Error() string { return e.category }

func cosFail(category string) error { return &cosFailure{category: category} }

func cosCategory(err error) string {
	var failure *cosFailure
	if errors.As(err, &failure) && failure.category != "" {
		return failure.category
	}
	if err == nil {
		return ""
	}
	return cosErrPutFailed
}

type cosSecret struct {
	AccessKeyID     string
	SecretAccessKey string
	Endpoint        string
	Region          string
	Bucket          string
}

type cosClientParams struct {
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	HTTPClient      *http.Client
}

// newCOSClient matches the S3-compatible client sub2api uses for Tencent COS:
// static credentials, an explicit base endpoint, virtual-hosted style, unsigned
// payload, and checksums only when the operation requires them. PutObject is a
// single call with ContentLength; there is no multipart upload.
func newCOSClient(ctx context.Context, p cosClientParams) (*s3.Client, error) {
	region := strings.TrimSpace(p.Region)
	if region == "" {
		region = cosDefaultRegion
	}
	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(p.AccessKeyID, p.SecretAccessKey, "")),
	}
	if p.HTTPClient != nil {
		loadOptions = append(loadOptions, awsconfig.WithHTTPClient(p.HTTPClient))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, cosFail(cosErrBucketUnavailable)
	}
	endpoint := strings.TrimSpace(p.Endpoint)
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = false
		o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	}), nil
}

type cosPresign struct {
	URL     string
	Method  string
	Headers map[string]string
}

type cosBucket interface {
	HeadBucket(ctx context.Context) error
	PutObject(ctx context.Context, key, contentType string, body []byte) error
	HeadObject(ctx context.Context, key string) (int64, error)
	GetObject(ctx context.Context, key string, limit int64) ([]byte, error)
	DeleteObject(ctx context.Context, key string) error
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (cosPresign, error)
}

type s3COSBucket struct {
	client *s3.Client
	bucket string
}

func openCOSBucket(ctx context.Context, secret cosSecret) (cosBucket, error) {
	client, err := newCOSClient(ctx, cosClientParams{
		Endpoint:        secret.Endpoint,
		Region:          secret.Region,
		AccessKeyID:     secret.AccessKeyID,
		SecretAccessKey: secret.SecretAccessKey,
	})
	if err != nil {
		return nil, err
	}
	return &s3COSBucket{client: client, bucket: secret.Bucket}, nil
}

func (b *s3COSBucket) HeadBucket(ctx context.Context) error {
	_, err := b.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(b.bucket)})
	if err != nil {
		return cosFail(cosErrBucketUnavailable)
	}
	return nil
}

func (b *s3COSBucket) PutObject(ctx context.Context, key, contentType string, body []byte) error {
	size := int64(len(body))
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return cosFail(cosErrPutFailed)
	}
	return nil
}

func (b *s3COSBucket) HeadObject(ctx context.Context, key string) (int64, error) {
	out, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return 0, cosFail(cosErrObjectMissing)
	}
	if out.ContentLength == nil {
		return 0, cosFail(cosErrSizeMismatch)
	}
	return *out.ContentLength, nil
}

func (b *s3COSBucket) GetObject(ctx context.Context, key string, limit int64) ([]byte, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, cosFail(cosErrObjectMissing)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(io.LimitReader(out.Body, limit+1))
	if err != nil {
		return nil, cosFail(cosErrObjectMissing)
	}
	if int64(len(data)) > limit {
		return nil, cosFail(cosErrManifestInvalid)
	}
	return data, nil
}

func (b *s3COSBucket) DeleteObject(ctx context.Context, key string) error {
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return cosFail(cosErrPutFailed)
	}
	return nil
}

func (b *s3COSBucket) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	presigner := s3.NewPresignClient(b.client)
	out, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", cosFail(cosErrPutFailed)
	}
	return out.URL, nil
}

func (b *s3COSBucket) PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (cosPresign, error) {
	presigner := s3.NewPresignClient(b.client)
	out, err := presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(key),
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return cosPresign{}, cosFail(cosErrPutFailed)
	}
	headers := make(map[string]string, len(out.SignedHeader))
	for name, values := range out.SignedHeader {
		if len(values) == 0 || strings.EqualFold(name, "Host") {
			continue
		}
		headers[name] = values[0]
	}
	return cosPresign{URL: out.URL, Method: out.Method, Headers: headers}, nil
}

func parseCOSSecret(payload []byte) (cosSecret, error) {
	var envelope struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || len(envelope.Data) == 0 {
		return cosSecret{}, cosFail(cosErrCredentialInvalid)
	}
	secret := cosSecret{
		AccessKeyID:     strings.TrimSpace(envelope.Data["access_key_id"]),
		SecretAccessKey: strings.TrimSpace(envelope.Data["secret_access_key"]),
		Endpoint:        strings.TrimSpace(envelope.Data["endpoint"]),
		Region:          strings.TrimSpace(envelope.Data["region"]),
		Bucket:          strings.TrimSpace(envelope.Data["bucket"]),
	}
	if secret.Endpoint == "" {
		secret.Endpoint = cosDefaultEndpoint
	}
	if secret.Region == "" {
		secret.Region = cosDefaultRegion
	}
	if secret.AccessKeyID == "" || secret.SecretAccessKey == "" || secret.Bucket == "" {
		return cosSecret{}, cosFail(cosErrCredentialInvalid)
	}
	if secret.Bucket != cosBucketName {
		return cosSecret{}, cosFail(cosErrBucketMismatch)
	}
	if !strings.HasPrefix(strings.ToLower(secret.Endpoint), "https://") {
		return cosSecret{}, cosFail(cosErrCredentialInvalid)
	}
	return secret, nil
}

func validCOSUserID(userID string) bool {
	if userID == "" || len(userID) > 80 {
		return false
	}
	for _, r := range userID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func cleanCOSRel(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || strings.Contains(rel, "\x00") || strings.Contains(rel, "\\") || strings.HasPrefix(rel, "/") {
		return "", cosFail(cosErrKeyRejected)
	}
	clean := path.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", cosFail(cosErrKeyRejected)
	}
	return clean, nil
}

func cosObjectKey(userID, sessionID, kind, rel string) (string, error) {
	if !validCOSUserID(userID) {
		return "", cosFail(cosErrKeyRejected)
	}
	if err := validateSession(sessionID); err != nil {
		return "", cosFail(cosErrKeyRejected)
	}
	if kind != "files" && kind != "snapshots" {
		return "", cosFail(cosErrKeyRejected)
	}
	clean, err := cleanCOSRel(rel)
	if err != nil {
		return "", err
	}
	if kind == "snapshots" && !validCOSSnapshotRel(clean) {
		return "", cosFail(cosErrKeyRejected)
	}
	prefix := "sessions/" + userID + "/" + sessionID + "/"
	key := prefix + kind + "/" + clean
	if !strings.HasPrefix(key, prefix) || strings.Contains(key, "/../") {
		return "", cosFail(cosErrKeyRejected)
	}
	return key, nil
}

func validCOSSnapshotRel(rel string) bool {
	if rel == "latest.json" {
		return true
	}
	commit, rest, ok := strings.Cut(rel, "/")
	if !ok || rest == "" {
		return false
	}
	for _, segment := range strings.Split(rest, "/") {
		if segment == ".." {
			return false
		}
	}
	if !immutableCommit(commit) {
		return false
	}
	return true
}

func immutableCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func cosControlObject(rel string) bool {
	if rel == "latest.json" {
		return true
	}
	const suffix = "/.doops/manifest.json"
	if !strings.HasSuffix(rel, suffix) {
		return false
	}
	commit := strings.TrimSuffix(rel, suffix)
	return immutableCommit(commit) && !strings.Contains(commit, "/")
}

type cosCall struct {
	ActorID    string
	Cluster    string
	Instance   string
	SessionID  string
	Credential string
	Operation  string
	Kind       string
	Path       string
	Size       int64
	Body       []byte
}

type cosResult struct {
	Storage         string            `json:"storage"`
	Bucket          string            `json:"bucket"`
	Key             string            `json:"key,omitempty"`
	URL             string            `json:"url,omitempty"`
	Method          string            `json:"method,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Size            int64             `json:"size,omitempty"`
	Commit          string            `json:"commit,omitempty"`
	ContentB64      string            `json:"content_b64,omitempty"`
	WorkspaceStatus string            `json:"workspace_status,omitempty"`
}

type cosSyncFile struct {
	Path       string `json:"path"`
	URL        string `json:"url"`
	Size       int64  `json:"size"`
	Executable *bool  `json:"executable"`
}

type cosManifest struct {
	Commit string             `json:"commit"`
	Files  []cosManifestEntry `json:"files"`
}

type cosManifestEntry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Executable *bool  `json:"executable"`
}

type cosService struct {
	load func(actorID, name, cluster, instance string) (cosSecret, error)
	open func(ctx context.Context, secret cosSecret) (cosBucket, error)
	sync func(ctx context.Context, cluster, instance, session string, files []cosSyncFile) (string, error)
}

func (s *cosService) Handle(ctx context.Context, call cosCall) (cosResult, error) {
	call.Operation = strings.TrimSpace(call.Operation)
	call.Kind = strings.TrimSpace(call.Kind)
	if s.load == nil || s.open == nil {
		return cosResult{}, cosFail(cosErrNotConfigured)
	}
	secret, err := s.load(call.ActorID, call.Credential, call.Cluster, call.Instance)
	if err != nil {
		return cosResult{}, err
	}
	bucket, err := s.open(ctx, secret)
	if err != nil {
		return cosResult{}, cosFail(cosCategory(err))
	}
	if err := bucket.HeadBucket(ctx); err != nil {
		return cosResult{}, cosFail(cosErrBucketUnavailable)
	}
	result := cosResult{Storage: "cos", Bucket: secret.Bucket}
	switch call.Operation {
	case "probe":
		return result, nil
	case "put":
		return s.put(ctx, bucket, call, result)
	case "presign_put":
		return s.presignPut(ctx, bucket, call, result)
	case "confirm":
		return s.confirm(ctx, bucket, call, result)
	case "presign_get":
		return s.presignGet(ctx, bucket, call, result)
	case "delete":
		return s.delete(ctx, bucket, call, result)
	case "get":
		return s.getControl(ctx, bucket, call, result)
	case "sync":
		return s.syncWorkspace(ctx, bucket, call, result)
	default:
		return cosResult{}, cosFail(cosErrCredentialInvalid)
	}
}

func (s *cosService) objectKey(call cosCall) (string, error) {
	return cosObjectKey(call.ActorID, call.SessionID, call.Kind, call.Path)
}

func (s *cosService) put(ctx context.Context, bucket cosBucket, call cosCall, result cosResult) (cosResult, error) {
	if int64(len(call.Body)) > cosGatewayPutMaxBytes {
		return cosResult{}, cosFail(cosErrGatewayPutLimit)
	}
	key, err := s.objectKey(call)
	if err != nil {
		return cosResult{}, err
	}
	if call.Kind == "snapshots" {
		rel, err := cleanCOSRel(call.Path)
		if err != nil {
			return cosResult{}, err
		}
		if strings.HasSuffix(rel, "/.doops/manifest.json") {
			if _, err := validateCOSManifest(call.Body, strings.TrimSuffix(rel, "/.doops/manifest.json")); err != nil {
				return cosResult{}, err
			}
		}
	}
	contentType := "application/octet-stream"
	if call.Kind == "snapshots" && (strings.HasSuffix(call.Path, "/.doops/manifest.json") || call.Path == "latest.json") {
		contentType = "application/json"
	}
	if err := bucket.PutObject(ctx, key, contentType, call.Body); err != nil {
		return cosResult{}, err
	}
	url, err := bucket.PresignGet(ctx, key, cosPresignGetTTL)
	if err != nil {
		return cosResult{}, err
	}
	result.Key = key
	result.URL = url
	result.Size = int64(len(call.Body))
	return result, nil
}

func (s *cosService) presignPut(ctx context.Context, bucket cosBucket, call cosCall, result cosResult) (cosResult, error) {
	if call.Size <= cosGatewayPutMaxBytes {
		return cosResult{}, cosFail(cosErrGatewayPutLimit)
	}
	key, err := s.objectKey(call)
	if err != nil {
		return cosResult{}, err
	}
	presign, err := bucket.PresignPut(ctx, key, "application/octet-stream", call.Size, cosPresignPutTTL)
	if err != nil {
		return cosResult{}, err
	}
	result.Key = key
	result.URL = presign.URL
	result.Method = presign.Method
	result.Headers = presign.Headers
	result.Size = call.Size
	return result, nil
}

func (s *cosService) confirm(ctx context.Context, bucket cosBucket, call cosCall, result cosResult) (cosResult, error) {
	if call.Size <= cosGatewayPutMaxBytes {
		return cosResult{}, cosFail(cosErrGatewayPutLimit)
	}
	key, err := s.objectKey(call)
	if err != nil {
		return cosResult{}, err
	}
	got, err := bucket.HeadObject(ctx, key)
	if err != nil {
		return cosResult{}, err
	}
	if got != call.Size {
		return cosResult{}, cosFail(cosErrSizeMismatch)
	}
	url, err := bucket.PresignGet(ctx, key, cosPresignGetTTL)
	if err != nil {
		return cosResult{}, err
	}
	result.Key = key
	result.URL = url
	result.Size = got
	return result, nil
}

func (s *cosService) presignGet(ctx context.Context, bucket cosBucket, call cosCall, result cosResult) (cosResult, error) {
	key, err := s.objectKey(call)
	if err != nil {
		return cosResult{}, err
	}
	url, err := bucket.PresignGet(ctx, key, cosPresignGetTTL)
	if err != nil {
		return cosResult{}, err
	}
	result.Key = key
	result.URL = url
	return result, nil
}

func (s *cosService) delete(ctx context.Context, bucket cosBucket, call cosCall, result cosResult) (cosResult, error) {
	key, err := s.objectKey(call)
	if err != nil {
		return cosResult{}, err
	}
	if err := bucket.DeleteObject(ctx, key); err != nil {
		return cosResult{}, err
	}
	result.Key = key
	return result, nil
}

func (s *cosService) getControl(ctx context.Context, bucket cosBucket, call cosCall, result cosResult) (cosResult, error) {
	if call.Kind != "snapshots" || !cosControlObject(call.Path) {
		return cosResult{}, cosFail(cosErrKeyRejected)
	}
	key, err := s.objectKey(call)
	if err != nil {
		return cosResult{}, err
	}
	body, err := bucket.GetObject(ctx, key, cosControlMaxBytes)
	if err != nil {
		return cosResult{}, err
	}
	result.Key = key
	result.Size = int64(len(body))
	result.ContentB64 = base64.StdEncoding.EncodeToString(body)
	return result, nil
}

func (s *cosService) syncWorkspace(ctx context.Context, bucket cosBucket, call cosCall, result cosResult) (cosResult, error) {
	if !immutableCommit(call.Path) {
		return cosResult{}, cosFail(cosErrKeyRejected)
	}
	manifestRel := call.Path + "/.doops/manifest.json"
	key, err := cosObjectKey(call.ActorID, call.SessionID, "snapshots", manifestRel)
	if err != nil {
		return cosResult{}, err
	}
	body, err := bucket.GetObject(ctx, key, cosControlMaxBytes)
	if err != nil {
		return cosResult{}, err
	}
	manifest, err := validateCOSManifest(body, call.Path)
	if err != nil {
		return cosResult{}, err
	}
	files := make([]cosSyncFile, 0, len(manifest.Files))
	for _, entry := range manifest.Files {
		rel, err := cleanCOSRel(entry.Path)
		if err != nil {
			return cosResult{}, err
		}
		objectKey, err := cosObjectKey(call.ActorID, call.SessionID, "snapshots", call.Path+"/"+rel)
		if err != nil {
			return cosResult{}, err
		}
		url, err := bucket.PresignGet(ctx, objectKey, cosPresignGetTTL)
		if err != nil {
			return cosResult{}, err
		}
		files = append(files, cosSyncFile{Path: rel, URL: url, Size: entry.Size, Executable: entry.Executable})
	}
	result.Commit = call.Path
	result.Key = key
	if s.sync == nil {
		result.WorkspaceStatus = cosErrWorkspaceNotSync
		return result, nil
	}
	status, err := s.sync(ctx, call.Cluster, call.Instance, call.SessionID, files)
	if err != nil {
		if cosCategory(err) == cosErrAgentSnapshotUnsupported {
			return cosResult{}, err
		}
		if cosCategory(err) == cosErrWorkspaceNotSync || strings.Contains(err.Error(), "target offline") {
			result.WorkspaceStatus = cosErrWorkspaceNotSync
			return result, nil
		}
		result.WorkspaceStatus = cosErrWorkspaceStale
		return result, nil
	}
	if status == "" {
		status = "synced"
	}
	result.WorkspaceStatus = status
	return result, nil
}

// Validate the complete snapshot before storing its manifest or dispatching
// any file. Missing metadata cannot safely mean "not executable".
func validateCOSManifest(body []byte, commit string) (cosManifest, error) {
	var manifest cosManifest
	if err := json.Unmarshal(body, &manifest); err != nil || manifest.Commit != commit {
		return cosManifest{}, cosFail(cosErrManifestInvalid)
	}
	for _, entry := range manifest.Files {
		if entry.Executable == nil {
			return cosManifest{}, cosFail(cosErrPermissionsMissing)
		}
		if entry.Size < 0 {
			return cosManifest{}, cosFail(cosErrManifestInvalid)
		}
		if _, err := cleanCOSRel(entry.Path); err != nil {
			return cosManifest{}, err
		}
	}
	return manifest, nil
}

func cosResultFields(result cosResult) (map[string]interface{}, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	return fields, nil
}

func decodeCOSBody(contentB64 string) ([]byte, error) {
	if strings.TrimSpace(contentB64) == "" {
		return []byte{}, nil
	}
	body, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		return nil, cosFail(cosErrCredentialInvalid)
	}
	return body, nil
}

func formatCOSError(err error) string {
	category := cosCategory(err)
	if category == cosErrPermissionsMissing {
		return category + ": push again from the original source to record executable files"
	}
	if category == "" {
		return cosErrPutFailed
	}
	return category
}
