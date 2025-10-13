# Version Management Guide

This document explains how to manage versions for the Upbot Operator project.

## Overview

The project uses a centralized version management system with the following components:

- **VERSION file**: Single source of truth for the current version
- **Makefile**: Contains version-related targets and build logic
- **Docker builds**: Automatically tagged with proper version information
- **Helm chart**: Synchronized with the main version
- **GitHub workflows**: Automated building, testing, and releasing

## Version Format

We follow [Semantic Versioning](https://semver.org/):
- **MAJOR.MINOR.PATCH** (e.g., 1.2.3)
- **MAJOR**: Breaking changes
- **MINOR**: New features (backward compatible)
- **PATCH**: Bug fixes (backward compatible)

## Version Management Commands

### Check Current Version

```bash
make version
```

### Bump Version

```bash
# Bump patch version (1.2.3 -> 1.2.4)
make version-bump-patch

# Bump minor version (1.2.3 -> 1.3.0)
make version-bump-minor

# Bump major version (1.2.3 -> 2.0.0)
make version-bump-major
```

### Update Helm Chart

```bash
make update-helm-chart
```

## Docker Image Management

### Build Images

```bash
# Build with current version
make docker-build

# Build and tag with version + latest
make docker-build-and-tag

# Push images
make docker-push
```

### Image Tags

Images are tagged as:
- `ghcr.io/upbothq/upbot-operator:VERSION` (e.g., `ghcr.io/upbothq/upbot-operator:1.2.3`)
- `ghcr.io/upbothq/upbot-operator:latest` (for releases)

## Automated Workflows

### Docker Build (`docker-build.yml`)

Triggers on:
- Push to `main` or `develop` branches
- Pull requests to `main`
- Git tags starting with `v`

Builds multi-architecture images and pushes to GitHub Container Registry.

### Release (`release.yml`)

Triggers on:
- Git tags starting with `v`
- Manual workflow dispatch

Creates a full release with:
- Docker images
- Helm chart
- GitHub release with assets
- Updated documentation

### Version Bump (`version-bump.yml`)

Manual workflow to:
- Bump version (patch/minor/major)
- Update Helm chart
- Commit changes

### Helm Chart Publishing (`publish-helm.yml`)

Triggers on:
- Changes to `dist/chart/**` or `VERSION` file
- Manual workflow dispatch

Updates and publishes Helm chart to GitHub Pages.

## Development Workflow

### 1. Feature Development

```bash
# Work on feature branch
git checkout -b feature/my-feature

# Make changes
# ...

# Test locally
make test
make docker-build

# Create PR
```

### 2. Preparing for Release

```bash
# Switch to main branch
git checkout main
git pull origin main

# Bump version (choose appropriate type)
make version-bump-patch  # or minor/major

# Update helm chart
make update-helm-chart

# Commit changes
git add VERSION dist/chart/Chart.yaml
git commit -m "chore: bump version to $(cat VERSION)"
git push origin main
```

### 3. Creating a Release

#### Option A: Git Tag (Recommended)

```bash
# Create and push tag
VERSION=$(cat VERSION)
git tag -a "v${VERSION}" -m "Release v${VERSION}"
git push origin "v${VERSION}"
```

#### Option B: Manual Workflow

Go to GitHub Actions → Release workflow → Run workflow

### 4. Hotfix Release

```bash
# Create hotfix branch from tag
git checkout -b hotfix/v1.2.4 v1.2.3

# Make fixes
# ...

# Bump patch version
make version-bump-patch

# Update helm chart  
make update-helm-chart

# Commit and tag
git add .
git commit -m "fix: critical bug fix"
VERSION=$(cat VERSION)
git tag -a "v${VERSION}" -m "Hotfix v${VERSION}"
git push origin hotfix/v1.2.4
git push origin "v${VERSION}"

# Merge back to main
git checkout main
git merge hotfix/v1.2.4
git push origin main
```

## Configuration

### Environment Variables

The following can be overridden:

```bash
# Registry for Docker images
REGISTRY=ghcr.io/upbothq

# Image name
IMAGE_NAME=upbot-operator

# Version (usually read from VERSION file)
VERSION=1.2.3
```

### Makefile Variables

```makefile
# Override in make commands
make docker-build IMG=myregistry/myimage:mytag
make docker-build REGISTRY=my-registry.com/myorg
```

## Troubleshooting

### Version Mismatch

If you see version consistency errors in CI:

```bash
make update-helm-chart
git add dist/chart/Chart.yaml
git commit -m "chore: sync helm chart version"
```

### Failed Docker Push

Ensure you have proper permissions to push to the registry:

```bash
# For GitHub Container Registry
echo $GITHUB_TOKEN | docker login ghcr.io -u USERNAME --password-stdin
```

### Missing Build Args

If Docker build fails with missing version info:

```bash
# Ensure you're using the make targets
make docker-build  # instead of docker build directly
```

## Best Practices

1. **Always use make targets** instead of direct docker/helm commands
2. **Test locally** before pushing version changes
3. **Use semantic versioning** correctly
4. **Keep VERSION file in sync** with Helm chart
5. **Create descriptive release notes** for GitHub releases
6. **Test the full release process** in a fork first