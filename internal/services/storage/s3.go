package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Options configure an S3-compatible bucket.
type S3Options struct {
	Endpoint  string // empty: AWS
	Bucket    string
	Region    string
	PathStyle bool
	AccessKey string
	SecretKey string
	// PartSize is the multipart chunk size (default 8 MiB). Memory per
	// upload is about PartSize × 2.
	PartSize int64
}

// S3Storage stores objects in an S3-compatible bucket (AWS, R2, MinIO).
type S3Storage struct {
	client   *s3.Client
	uploader *manager.Uploader
	bucket   string
}

func NewS3Storage(o S3Options) *S3Storage {
	region := o.Region
	if region == "" {
		region = "auto"
	}
	opts := s3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(o.AccessKey, o.SecretKey, ""),
		UsePathStyle: o.PathStyle,
		// R2 and older MinIO reject the SDK's default CRC32 trailers.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if o.Endpoint != "" {
		opts.BaseEndpoint = aws.String(o.Endpoint)
	}
	client := s3.New(opts)
	part := o.PartSize
	if part <= 0 {
		part = 8 << 20
	}
	up := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = part
		u.Concurrency = 2
		u.LeavePartsOnError = false
	})
	return &S3Storage{client: client, uploader: up, bucket: o.Bucket}
}

func (s *S3Storage) Driver() string { return "s3" }

// Put streams r through the multipart uploader (one buffered part at a time
// per worker; a body under PartSize is a single PutObject).
func (s *S3Storage) Put(ctx context.Context, key string, r io.Reader, size int64, meta Meta) error {
	cr := &checkedReader{r: io.LimitReader(r, size), src: r, want: size}
	in := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   cr,
	}
	if meta.ContentType != "" {
		in.ContentType = aws.String(meta.ContentType)
	}
	if meta.ContentDisposition != "" {
		in.ContentDisposition = aws.String(meta.ContentDisposition)
	}
	if meta.CacheControl != "" {
		in.CacheControl = aws.String(meta.CacheControl)
	}
	_, err := s.uploader.Upload(ctx, in)
	if cr.err != nil {
		// The body was wrong: don't keep what got stored.
		_ = s.Delete(context.WithoutCancel(ctx), key)
		return cr.err
	}
	return err
}

// checkedReader reads exactly want bytes from r (a LimitReader over src)
// and records a short or long body as err so Put fails.
type checkedReader struct {
	r    io.Reader
	src  io.Reader
	want int64
	n    int64
	err  error
}

func (c *checkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err == io.EOF {
		if c.n < c.want {
			c.err = ErrShortBody
			return n, c.err
		}
		var one [1]byte
		if m, _ := io.ReadFull(c.src, one[:]); m > 0 {
			c.err = ErrLongBody
			return n, c.err
		}
		return n, io.EOF
	}
	if err != nil {
		c.err = err
	}
	return n, err
}

func (s *S3Storage) Open(ctx context.Context, key string, size int64) (io.ReadSeekCloser, error) {
	if size < 0 {
		head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
		if err != nil {
			return nil, mapNotFound(err)
		}
		size = aws.ToInt64(head.ContentLength)
	}
	return &s3Reader{ctx: ctx, s: s, key: key, size: size}, nil
}

func (s *S3Storage) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if errors.Is(mapNotFound(err), ErrNotFound) {
		return nil
	}
	return err
}

// Probe checks the bucket exists and the keys may use it.
func (s *S3Storage) Probe(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	if err != nil {
		return fmt.Errorf("storage: bucket %s: %w", s.bucket, err)
	}
	return nil
}

func mapNotFound(err error) error {
	if err == nil {
		return nil
	}
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return ErrNotFound
	}
	var ae smithy.APIError
	if errors.As(err, &ae) && (ae.ErrorCode() == "NoSuchKey" || ae.ErrorCode() == "NotFound") {
		return ErrNotFound
	}
	return err
}

// s3Reader is a lazily opened, seekable view of an object: each read after
// a seek issues a ranged GetObject from the current offset, so
// http.ServeContent's Range handling streams only what was asked.
type s3Reader struct {
	ctx     context.Context
	s       *S3Storage
	key     string
	size    int64
	pos     int64
	body    io.ReadCloser
	bodyPos int64
}

func (r *s3Reader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.body == nil || r.bodyPos != r.pos {
		if r.body != nil {
			_ = r.body.Close()
			r.body = nil
		}
		out, err := r.s.client.GetObject(r.ctx, &s3.GetObjectInput{
			Bucket: aws.String(r.s.bucket),
			Key:    aws.String(r.key),
			Range:  aws.String("bytes=" + strconv.FormatInt(r.pos, 10) + "-"),
		})
		if err != nil {
			return 0, mapNotFound(err)
		}
		r.body, r.bodyPos = out.Body, r.pos
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	r.bodyPos += int64(n)
	if err == io.EOF && r.pos < r.size {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *s3Reader) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.pos + offset
	case io.SeekEnd:
		next = r.size + offset
	default:
		return 0, errors.New("storage: invalid whence")
	}
	if next < 0 {
		return 0, errors.New("storage: negative position")
	}
	r.pos = next
	return next, nil
}

func (r *s3Reader) Close() error {
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}
