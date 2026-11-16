package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCOSObjectKeyStaysInsideCallerPrefix(t *testing.T) {
	const userID = "usr_caller"
	const sessionID = "sess_1"
	key, err := cosObjectKey(userID, sessionID, "files", "notes/a.txt")
	if err != nil || key != "sessions/usr_caller/sess_1/files/notes/a.txt" {
		t.Fatalf("key = %q, %v", key, err)
	}
	manifest := strings.Repeat("a", 40) + "/.doops/manifest.json"
	if key, err = cosObjectKey(userID, sessionID, "snapshots", manifest); err != nil || !cosControlObject(manifest) {
		t.Fatalf("manifest key = %q, %v", key, err)
	}
	for _, rel := range []string{"../usr_other/sess_1/files/secret", "/etc/passwd", "foo/../../outside", `..\outside`, ""} {
		if _, err := cosObjectKey(userID, sessionID, "files", rel); cosCategory(err) != cosErrKeyRejected {
			t.Fatalf("rel %q error = %v, want %s", rel, err, cosErrKeyRejected)
		}
	}
	if _, err := cosObjectKey("usr_other", sessionID, "files", "notes/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := cosObjectKey("../usr_other", sessionID, "files", "a.txt"); cosCategory(err) != cosErrKeyRejected {
		t.Fatalf("unsafe user id error = %v", err)
	}
}

func TestCOSSnapshotLiteralDotsDoNotEscapeCallerPrefix(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for _, name := range []string{"app/api/admin/[...path]/route.ts", "app/api/credentials/[[...path]]/route.ts", "notes/backup..txt"} {
		rel := commit + "/" + name
		key, err := cosObjectKey("usr_caller", "snapshot", "snapshots", rel)
		if err != nil || key != "sessions/usr_caller/snapshot/snapshots/"+rel {
			t.Fatalf("literal filename %q: key=%q err=%v", name, key, err)
		}
	}
	for _, name := range []string{"..", "../outside", "app/../outside", "app/.."} {
		if validCOSSnapshotRel(commit + "/" + name) {
			t.Fatalf("traversal segment admitted: %q", name)
		}
	}
}

func TestParseCOSSecretRejectsWrongBucketWithoutEchoingSecret(t *testing.T) {
	const secret = "synthetic-cos-secret"
	payload := []byte(`{"data":{"access_key_id":"AKID","secret_access_key":"` + secret + `","endpoint":"https://cos.ap-guangzhou.myqcloud.com","region":"ap-guangzhou","bucket":"other-bucket"}}`)
	_, err := parseCOSSecret(payload)
	if cosCategory(err) != cosErrBucketMismatch {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error exposed credential material: %v", err)
	}
	_, err = parseCOSSecret([]byte(`{"data":{"bucket":"doops-1253657668"}}`))
	if cosCategory(err) != cosErrCredentialInvalid {
		t.Fatalf("missing key error = %v", err)
	}
}

func TestCOSPutBoundaryUsesSinglePutOrPresign(t *testing.T) {
	bucket := &fakeCOSBucket{}
	service := cosService{
		load: func(string, string, string, string) (cosSecret, error) {
			return cosSecret{Bucket: cosBucketName, Endpoint: cosDefaultEndpoint, Region: cosDefaultRegion, AccessKeyID: "id", SecretAccessKey: "synthetic-cos-secret"}, nil
		},
		open: func(context.Context, cosSecret) (cosBucket, error) { return bucket, nil },
	}
	call := cosCall{
		ActorID: "usr_caller", Cluster: "dev", Instance: "local", SessionID: "sess_1",
		Credential: "cos-bucket", Kind: "files", Path: "a.txt",
	}
	call.Operation = "put"
	call.Body = bytes.Repeat([]byte("a"), int(cosGatewayPutMaxBytes))
	result, err := service.Handle(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if len(bucket.puts) != 1 || bucket.puts[0].Size != cosGatewayPutMaxBytes || bucket.puts[0].ContentLength != cosGatewayPutMaxBytes {
		t.Fatalf("put boundary = %#v", bucket.puts)
	}
	if result.URL == "" || !strings.HasPrefix(result.Key, "sessions/usr_caller/sess_1/files/") {
		t.Fatalf("result = %#v", result)
	}
	if strings.Contains(result.URL, "synthetic-cos-secret") {
		t.Fatalf("presigned result exposed secret: %#v", result)
	}

	bucket.puts = nil
	call.Body = bytes.Repeat([]byte("b"), int(cosGatewayPutMaxBytes)+1)
	if _, err := service.Handle(context.Background(), call); cosCategory(err) != cosErrGatewayPutLimit || len(bucket.puts) != 0 {
		t.Fatalf("oversize put error = %v, puts = %d", err, len(bucket.puts))
	}

	call.Operation = "presign_put"
	call.Body = nil
	call.Size = cosGatewayPutMaxBytes
	if _, err := service.Handle(context.Background(), call); cosCategory(err) != cosErrGatewayPutLimit || len(bucket.presignPuts) != 0 {
		t.Fatalf("presign at boundary error = %v", err)
	}
	call.Size = cosGatewayPutMaxBytes + 1
	presign, err := service.Handle(context.Background(), call)
	if err != nil || len(bucket.presignPuts) != 1 || bucket.presignPuts[0] != call.Size || presign.Method != http.MethodPut {
		t.Fatalf("presign = %#v, err = %v, recorded = %#v", presign, err, bucket.presignPuts)
	}
}

func TestGatewayCOSConfigIsAuthoritative(t *testing.T) {
	t.Setenv("DOOPS_COS_ACCESS_KEY_ID", "")
	t.Setenv("DOOPS_COS_SECRET_ACCESS_KEY", "")
	if _, err := loadGatewayCOSConfig(); cosCategory(err) != cosErrNotConfigured {
		t.Fatalf("missing gateway config error = %v", err)
	}
	t.Setenv("DOOPS_COS_ACCESS_KEY_ID", "AKID")
	t.Setenv("DOOPS_COS_SECRET_ACCESS_KEY", "synthetic-cos-secret")
	t.Setenv("DOOPS_COS_ENDPOINT", "https://cos.ap-guangzhou.myqcloud.com")
	t.Setenv("DOOPS_COS_REGION", "ap-guangzhou")
	t.Setenv("DOOPS_COS_BUCKET", "")
	secret, err := loadGatewayCOSConfig()
	if err != nil || secret.Bucket != cosBucketName || secret.Endpoint != cosDefaultEndpoint {
		t.Fatalf("gateway config = %#v, %v", secret, err)
	}
	if strings.Contains(fmt.Sprint(err), "synthetic-cos-secret") {
		t.Fatal("config error exposed the secret")
	}
	bucket := &fakeCOSBucket{}
	service := cosService{
		load: func(_, name, _, _ string) (cosSecret, error) {
			if name != "caller-picked" {
				t.Fatalf("loader saw credential %q", name)
			}
			return loadGatewayCOSConfig()
		},
		open: func(context.Context, cosSecret) (cosBucket, error) { return bucket, nil },
	}
	result, err := service.Handle(context.Background(), cosCall{
		ActorID: "usr_caller", Cluster: "dev", Instance: "local", SessionID: "sess_1",
		Credential: "caller-picked", Operation: "probe",
	})
	if err != nil || result.Bucket != cosBucketName || result.Storage != "cos" {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestCOSHeadBucketFailureStaysOnCOS(t *testing.T) {
	bucket := &fakeCOSBucket{headErr: cosFail(cosErrBucketUnavailable)}
	service := cosService{
		load: func(string, string, string, string) (cosSecret, error) {
			return cosSecret{Bucket: cosBucketName, AccessKeyID: "id", SecretAccessKey: "synthetic-cos-secret"}, nil
		},
		open: func(context.Context, cosSecret) (cosBucket, error) { return bucket, nil },
	}
	_, err := service.Handle(context.Background(), cosCall{
		ActorID: "usr_caller", Cluster: "dev", Instance: "local", SessionID: "sess_1",
		Credential: "cos-bucket", Operation: "put", Kind: "files", Path: "a.txt", Body: []byte("x"),
	})
	if cosCategory(err) != cosErrBucketUnavailable || len(bucket.puts) != 0 {
		t.Fatalf("error = %v, puts = %d", err, len(bucket.puts))
	}
	if strings.Contains(err.Error(), "synthetic-cos-secret") {
		t.Fatal("bucket error exposed secret")
	}
}

func TestGatewayRPCRejectsMissingToken(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	req, err := http.NewRequest(http.MethodGet, f.server.URL+"/v1/rpc?cluster=dev&instance=local", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "gateway_auth_missing") {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

func TestCOSSyncWritesPresignedObjectIntoWorkspace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	payload := []byte("snapshot-bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "127.0.0.1" && !strings.HasPrefix(r.Host, "127.0.0.1:") {
			t.Errorf("unexpected host %s", r.Host)
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	text, err := handleCOSSync(mustJSON(t, map[string]interface{}{
		"session_id": "sess_1",
		"files": []map[string]interface{}{
			{"path": "dir/a.txt", "url": server.URL + "/a.txt", "size": len(payload), "executable": false},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "synced 1") {
		t.Fatalf("sync result = %s", text)
	}
	got, err := os.ReadFile(filepath.Join(root, "sess_1", "dir", "a.txt"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("workspace bytes = %q, %v", got, err)
	}
	if _, err := handleCOSSync(mustJSON(t, map[string]interface{}{
		"session_id": "sess_1",
		"files":      []map[string]interface{}{{"path": "../outside", "url": server.URL, "size": 1, "executable": false}},
	})); err == nil {
		t.Fatal("sync accepted a path outside the session")
	}
}

func TestCOSClientPutObjectUsesUnsignedPayloadAndContentLength(t *testing.T) {
	const secret = "synthetic-cos-secret"
	transport := &captureTransport{}
	client, err := newCOSClient(context.Background(), cosClientParams{
		Endpoint:        cosDefaultEndpoint,
		Region:          cosDefaultRegion,
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: secret,
		HTTPClient:      &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("payload-bytes")
	bucket := &s3COSBucket{client: client, bucket: cosBucketName}
	if err := bucket.PutObject(context.Background(), "sessions/usr_caller/sess_1/files/a.txt", "application/octet-stream", body); err != nil {
		t.Fatal(err)
	}
	req := transport.req
	if req == nil {
		t.Fatal("no request captured")
	}
	if req.URL.Scheme != "https" || req.URL.Host != cosBucketName+".cos.ap-guangzhou.myqcloud.com" {
		t.Fatalf("endpoint = %s", req.URL.String())
	}
	if strings.Contains(req.URL.Path, cosBucketName) {
		t.Fatalf("path-style request = %s", req.URL.String())
	}
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != "UNSIGNED-PAYLOAD" {
		t.Fatalf("payload hash = %q", got)
	}
	if req.ContentLength != int64(len(body)) || !bytes.Equal(transport.body, body) {
		t.Fatalf("content length = %d, body = %d", req.ContentLength, len(transport.body))
	}
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-checksum") || strings.EqualFold(name, "x-amz-sdk-checksum-algorithm") {
			t.Fatalf("checksum header %s was sent", name)
		}
	}
	if strings.Contains(req.URL.String(), secret) || strings.Contains(fmtHeader(req.Header), secret) {
		t.Fatal("request exposed secret access key")
	}
}

type recordedPut struct {
	Key           string
	Size          int64
	ContentLength int64
}

type fakeCOSBucket struct {
	headErr     error
	puts        []recordedPut
	presignPuts []int64
	objects     map[string][]byte
}

func (b *fakeCOSBucket) HeadBucket(context.Context) error { return b.headErr }

func (b *fakeCOSBucket) PutObject(_ context.Context, key, _ string, body []byte) error {
	b.puts = append(b.puts, recordedPut{Key: key, Size: int64(len(body)), ContentLength: int64(len(body))})
	if b.objects == nil {
		b.objects = map[string][]byte{}
	}
	b.objects[key] = append([]byte(nil), body...)
	return nil
}

func (b *fakeCOSBucket) HeadObject(_ context.Context, key string) (int64, error) {
	body, ok := b.objects[key]
	if !ok {
		return 0, cosFail(cosErrObjectMissing)
	}
	return int64(len(body)), nil
}

func (b *fakeCOSBucket) GetObject(_ context.Context, key string, limit int64) ([]byte, error) {
	body, ok := b.objects[key]
	if !ok {
		return nil, cosFail(cosErrObjectMissing)
	}
	if int64(len(body)) > limit {
		return nil, cosFail(cosErrManifestInvalid)
	}
	return append([]byte(nil), body...), nil
}

func (b *fakeCOSBucket) DeleteObject(_ context.Context, key string) error {
	delete(b.objects, key)
	return nil
}

func (b *fakeCOSBucket) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	if ttl != cosPresignGetTTL {
		return "", cosFail(cosErrPutFailed)
	}
	return "https://example.invalid/" + key + "?presigned=1", nil
}

func (b *fakeCOSBucket) PresignPut(_ context.Context, key, _ string, size int64, _ time.Duration) (cosPresign, error) {
	b.presignPuts = append(b.presignPuts, size)
	return cosPresign{URL: "https://example.invalid/" + key, Method: http.MethodPut, Headers: map[string]string{"Content-Type": "application/octet-stream"}}, nil
}

type captureTransport struct {
	req  *http.Request
	body []byte
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	c.body = body
	cloned := req.Clone(req.Context())
	cloned.Body = io.NopCloser(bytes.NewReader(body))
	cloned.ContentLength = req.ContentLength
	cloned.Header = req.Header.Clone()
	c.req = cloned
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader("")),
		ContentLength: 0,
		Request:       req,
	}, nil
}

func fmtHeader(header http.Header) string {
	var buf strings.Builder
	_ = header.Write(&buf)
	return buf.String()
}
