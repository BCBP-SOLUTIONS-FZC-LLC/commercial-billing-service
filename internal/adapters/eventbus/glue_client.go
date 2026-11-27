package eventbus

import (
	"context"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/glue"
)

// NewGlueCodecFor builds the Glue encode codec for region and endpoint (the
// values platform-events' config.LoadSNS resolved). platform-events ships
// only a decode codec (events.GlueDecodeCodec), so the encoder is Billing's,
// plugged into platform-events' SNS publisher through events.WithCodec; this
// package is the only one that names the AWS SDK (depguard).
func NewGlueCodecFor(ctx context.Context, region, endpoint, registry string) (*GlueCodec, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	var gopts []func(*glue.Options)
	if endpoint != "" {
		gopts = append(gopts, func(o *glue.Options) { o.BaseEndpoint = &endpoint })
	}
	return NewGlueCodec(ctx, glue.NewFromConfig(awsCfg, gopts...), registry)
}
