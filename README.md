# GitHub Prometheus Exporter

A Prometheus exporter that collects metrics from GitHub, including:

- Issue and pull request counts
- Notification counts
- Workflow run states and numbers

## Usage

The exporter requires a GitHub personal access token to function. Set it via the `GITHUB_TOKEN` environment variable or using the `--token` flag.

### Serve Mode

Run as a Prometheus metrics endpoint:

```bash
github_exporter serve [options]

Options:
  -h, --host      Host address to listen on (default: ":9100")
  -i, --interval  Metrics collection interval (default: 15m)
```

Metrics will be available at `http://localhost:9100/metrics`

### Generate Mode

Generate metrics once and exit:

```bash
github_exporter generate [options]

Options:
  -o, --output      Output file path (defaults to stdout if not specified)
  -p, --pushgateway Pushgateway URL to send metrics to
  -r, --pushgateway-retries Number of retries for Pushgateway requests (default: 1)
```

### Environment Variables

All CLI options can be configured via environment variables:

- `GITHUB_TOKEN`: GitHub personal access token (required)
- `GITHUB_EXPORTER_VERBOSE`: Enable verbose logging
- `GITHUB_EXPORTER_HOST`: Host address for serve mode
- `GITHUB_EXPORTER_INTERVAL`: Collection interval for serve mode
- `GITHUB_EXPORTER_OUTPUT`: Output file path for generate mode
- `GITHUB_EXPORTER_PUSHGATEWAY_URL`: Pushgateway URL for generate mode
- `GITHUB_EXPORTER_PUSHGATEWAY_RETRIES`: Number of retries for Pushgateway requests (default: 1)
