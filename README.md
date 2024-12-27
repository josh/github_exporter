# GitHub Metrics Exporter

A Prometheus metrics exporter for GitHub that collects various metrics from your GitHub account and repositories.

## Metrics Collected

- Issue and Pull Request counts (open/closed) for each repository
- Unread notification count
- Workflow run statistics (latest run number and conclusion state)

## Usage

1. Set your GitHub token as an environment variable:

   ```bash
   export GITHUB_TOKEN=your_github_token
   ```

2. Run the exporter:
   ```bash
   ./github_exporter
   ```

The exporter will collect metrics and write them to `metrics.prom` in Prometheus text format.

## License

MIT License - See LICENSE file for details
