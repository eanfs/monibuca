package plugin_s3

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"google.golang.org/protobuf/types/known/emptypb"
	gpb "m7s.live/v5/pb"
	"m7s.live/v5/plugin/s3/pb"
)

// Upload implements the gRPC Upload method
func (p *S3Plugin) Upload(ctx context.Context, req *pb.UploadRequest) (*pb.UploadResponse, error) {
	if req.Filename == "" {
		return nil, fmt.Errorf("filename is required")
	}
	if len(req.Content) == 0 {
		return nil, fmt.Errorf("content is required")
	}

	bucket := req.Bucket
	if bucket == "" {
		bucket = p.Bucket
	}

	// Generate S3 key
	key := req.Filename
	if !strings.HasPrefix(key, "/") {
		key = "/" + key
	}

	// Determine content type
	contentType := req.ContentType
	if contentType == "" {
		contentType = http.DetectContentType(req.Content)
	}

	// Upload to S3
	input := &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(req.Content),
		ContentLength: aws.Int64(int64(len(req.Content))),
		ContentType:   aws.String(contentType),
	}

	result, err := p.s3Client.PutObjectWithContext(ctx, input)
	if err != nil {
		p.Error("Failed to upload file to S3", "error", err, "key", key, "bucket", bucket)
		return nil, fmt.Errorf("failed to upload file: %v", err)
	}

	// Generate public URL
	url := fmt.Sprintf("%s/%s%s", p.getEndpointURL(), bucket, key)

	p.Info("File uploaded successfully", "key", key, "bucket", bucket, "size", len(req.Content))

	return &pb.UploadResponse{
		Code:    0,
		Message: "Upload successful",
		Data: &pb.UploadData{
			Key:  key,
			Url:  url,
			Size: int64(len(req.Content)),
			Etag: aws.StringValue(result.ETag),
		},
	}, nil
}

// List implements the gRPC List method
func (p *S3Plugin) List(ctx context.Context, req *pb.ListRequest) (*pb.ListResponse, error) {
	bucket := req.Bucket
	if bucket == "" {
		bucket = p.Bucket
	}

	input := &s3.ListObjectsInput{
		Bucket: aws.String(bucket),
	}

	if req.Prefix != "" {
		input.Prefix = aws.String(req.Prefix)
	}
	if req.MaxKeys > 0 {
		input.MaxKeys = aws.Int64(int64(req.MaxKeys))
	}
	if req.Marker != "" {
		input.Marker = aws.String(req.Marker)
	}

	result, err := p.s3Client.ListObjectsWithContext(ctx, input)
	if err != nil {
		p.Error("Failed to list objects from S3", "error", err, "bucket", bucket)
		return nil, fmt.Errorf("failed to list objects: %v", err)
	}

	var objects []*pb.S3Object
	for _, obj := range result.Contents {
		objects = append(objects, &pb.S3Object{
			Key:          aws.StringValue(obj.Key),
			Size:         aws.Int64Value(obj.Size),
			LastModified: obj.LastModified.Format(time.RFC3339),
			Etag:         aws.StringValue(obj.ETag),
			StorageClass: aws.StringValue(obj.StorageClass),
		})
	}

	var nextMarker string
	if result.NextMarker != nil {
		nextMarker = aws.StringValue(result.NextMarker)
	}

	p.Info("Listed objects successfully", "bucket", bucket, "count", len(objects))

	return &pb.ListResponse{
		Code:    0,
		Message: "List successful",
		Data: &pb.ListData{
			Objects:     objects,
			IsTruncated: aws.BoolValue(result.IsTruncated),
			NextMarker:  nextMarker,
		},
	}, nil
}

// Delete implements the gRPC Delete method
func (p *S3Plugin) Delete(ctx context.Context, req *pb.DeleteRequest) (*gpb.SuccessResponse, error) {
	if req.Key == "" {
		return nil, fmt.Errorf("key is required")
	}

	bucket := req.Bucket
	if bucket == "" {
		bucket = p.Bucket
	}

	input := &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(req.Key),
	}

	_, err := p.s3Client.DeleteObjectWithContext(ctx, input)
	if err != nil {
		p.Error("Failed to delete object from S3", "error", err, "key", req.Key, "bucket", bucket)
		return nil, fmt.Errorf("failed to delete object: %v", err)
	}

	p.Info("Object deleted successfully", "key", req.Key, "bucket", bucket)

	return &gpb.SuccessResponse{
		Code:    0,
		Message: "Delete successful",
	}, nil
}

// CheckConnection implements the gRPC CheckConnection method
func (p *S3Plugin) CheckConnection(ctx context.Context, req *emptypb.Empty) (*pb.ConnectionResponse, error) {
	// Test connection by listing buckets
	_, err := p.s3Client.ListBucketsWithContext(ctx, &s3.ListBucketsInput{})

	connected := err == nil
	message := "Connection successful"
	if err != nil {
		message = fmt.Sprintf("Connection failed: %v", err)
		p.Error("S3 connection check failed", "error", err)
	} else {
		p.Info("S3 connection check successful")
	}

	return &pb.ConnectionResponse{
		Code:    0,
		Message: message,
		Data: &pb.ConnectionData{
			Connected: connected,
			Endpoint:  p.Endpoint,
			Region:    p.Region,
			UseSsl:    p.UseSSL,
			Bucket:    p.Bucket,
		},
	}, nil
}

// Helper method to get endpoint URL
func (p *S3Plugin) getEndpointURL() string {
	protocol := "http"
	if p.UseSSL {
		protocol = "https"
	}
	return fmt.Sprintf("%s://%s", protocol, p.Endpoint)
}
