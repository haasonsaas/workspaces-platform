package artifacts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type S3Store struct {
	client    *s3.Client
	presigner *s3.PresignClient

	bucket string
	prefix string

	publishLinks bool
	presignTTL   time.Duration
}

type S3Config struct {
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	Prefix          string
	ForcePathStyle  bool

	PublishLinks bool
	PresignTTL   time.Duration
}

func NewS3FromEnv(ctx context.Context) (*S3Store, error) {
	return NewS3FromEnvWithPrefix(ctx, "ARTIFACT_")
}

// NewS3FromEnvWithPrefix reads S3 config from environment variables using a prefix.
//
// Expected variables (prefix defaults shown for "ARTIFACT_"):
// - ARTIFACT_S3_ENDPOINT
// - ARTIFACT_S3_BUCKET
// - ARTIFACT_S3_REGION (default: us-east-1)
// - ARTIFACT_S3_ACCESS_KEY_ID
// - ARTIFACT_S3_SECRET_ACCESS_KEY
// - ARTIFACT_S3_PREFIX (default: workspaces)
// - ARTIFACT_S3_FORCE_PATH_STYLE (default: true)
// - ARTIFACT_PUBLISH_LINKS (default: false)
// - ARTIFACT_PRESIGN_TTL_SECONDS (default: 86400)
func NewS3FromEnvWithPrefix(ctx context.Context, prefix string) (*S3Store, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil, errors.New("empty env prefix")
	}

	endpoint := strings.TrimSpace(os.Getenv(prefix + "S3_ENDPOINT"))
	bucket := strings.TrimSpace(os.Getenv(prefix + "S3_BUCKET"))
	if endpoint == "" || bucket == "" {
		return nil, fmt.Errorf("%sS3_ENDPOINT and %sS3_BUCKET are required", prefix, prefix)
	}
	region := strings.TrimSpace(getenv(prefix+"S3_REGION", "us-east-1"))

	ak := strings.TrimSpace(os.Getenv(prefix + "S3_ACCESS_KEY_ID"))
	sk := strings.TrimSpace(os.Getenv(prefix + "S3_SECRET_ACCESS_KEY"))
	if ak == "" || sk == "" {
		return nil, fmt.Errorf("%sS3_ACCESS_KEY_ID and %sS3_SECRET_ACCESS_KEY are required", prefix, prefix)
	}

	prefixPath := strings.Trim(strings.TrimSpace(getenv(prefix+"S3_PREFIX", "workspaces")), "/")
	forcePathStyle := strings.EqualFold(strings.TrimSpace(getenv(prefix+"S3_FORCE_PATH_STYLE", "true")), "true")

	publishLinks := strings.EqualFold(strings.TrimSpace(getenv(prefix+"PUBLISH_LINKS", "false")), "true")
	presignTTL := 24 * time.Hour
	if raw := strings.TrimSpace(getenv(prefix+"PRESIGN_TTL_SECONDS", "86400")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			presignTTL = time.Duration(n) * time.Second
		}
	}

	return NewS3(ctx, S3Config{
		Endpoint:        endpoint,
		Region:          region,
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		Bucket:          bucket,
		Prefix:          prefixPath,
		ForcePathStyle:  forcePathStyle,
		PublishLinks:    publishLinks,
		PresignTTL:      presignTTL,
	})
}

func NewS3(ctx context.Context, cfg S3Config) (*S3Store, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" || strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("endpoint and bucket are required")
	}
	if strings.TrimSpace(cfg.Region) == "" {
		cfg.Region = "us-east-1"
	}
	if strings.TrimSpace(cfg.AccessKeyID) == "" || strings.TrimSpace(cfg.SecretAccessKey) == "" {
		return nil, errors.New("access key and secret are required")
	}
	if strings.TrimSpace(cfg.Prefix) == "" {
		cfg.Prefix = "workspaces"
	}

	if _, err := url.Parse(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("invalid endpoint: %w", err)
	}

	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(cfg.Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(func(service, region string, _ ...any) (aws.Endpoint, error) {
			if service == s3.ServiceID {
				return aws.Endpoint{
					URL:               cfg.Endpoint,
					HostnameImmutable: true,
					SigningRegion:     cfg.Region,
				}, nil
			}
			return aws.Endpoint{}, &aws.EndpointNotFoundError{}
		})),
	)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = cfg.ForcePathStyle
	})

	return &S3Store{
		client:       client,
		presigner:    s3.NewPresignClient(client),
		bucket:       cfg.Bucket,
		prefix:       strings.Trim(cfg.Prefix, "/"),
		publishLinks: cfg.PublishLinks,
		presignTTL:   cfg.PresignTTL,
	}, nil
}

func (s *S3Store) Enabled() bool { return s != nil && s.client != nil && s.bucket != "" }

func (s *S3Store) Key(parts ...string) string {
	p := []string{s.prefix}
	for _, part := range parts {
		t := strings.Trim(strings.TrimSpace(part), "/")
		if t == "" {
			continue
		}
		p = append(p, t)
	}
	return strings.Join(p, "/")
}

func (s *S3Store) Put(ctx context.Context, key string, contentType string, body []byte) error {
	if !s.Enabled() {
		return errors.New("artifact store not configured")
	}
	key = strings.TrimLeft(strings.TrimSpace(key), "/")
	if key == "" {
		return errors.New("empty key")
	}
	ct := strings.TrimSpace(contentType)
	if ct == "" {
		ct = "application/octet-stream"
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(ct),
		ACL:         s3types.ObjectCannedACLPrivate,
	})
	return err
}

func (s *S3Store) PresignGet(ctx context.Context, key string) (string, bool, error) {
	if !s.Enabled() {
		return "", false, errors.New("artifact store not configured")
	}
	if !s.publishLinks {
		return "", false, nil
	}
	key = strings.TrimLeft(strings.TrimSpace(key), "/")
	if key == "" {
		return "", false, errors.New("empty key")
	}
	req, err := s.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(s.presignTTL))
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(req.URL), true, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
