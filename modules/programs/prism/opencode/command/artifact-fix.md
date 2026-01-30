# Quick Guide: Enable Container Artifact Extraction

This guide shows how to enable artifact extraction from container builds for CI/CD pipelines.

## Overview

Artifact extraction allows CI/CD systems to extract build outputs (JARs, distributions, etc.) from container images without needing to install language-specific tools on the build agents.

## Steps

### 1. Create Feature Branch
```bash
git checkout -b enable-artifact-extraction
```

### 2. Update GitHub Workflow (if using reusable workflows)

If your workflow has artifact extraction disabled, update `.github/workflows/[workflow-name].yml`:

```yaml
# Look for and remove/update lines like:
# create-build-artifacts: false

# Ensure artifact extraction is enabled (usually default):
jobs:
  build-and-release:
    uses: firstcape-digital/github-actions/.github/workflows/build-release-container-image.yml@v1
    with:
      # ... other parameters
      # Artifact extraction is enabled by default - no extra parameter needed
```

### 3. Add Artifacts Stage to Dockerfile/Containerfile

Add this stage **after** your build stage but **before** your production stage:

#### **SBT/Scala Projects**
```dockerfile
# Create a clean artifacts stage for artifact upload
FROM scratch AS artifacts
COPY --from=builder /app/target/universal/stage/ /artifacts/
```

#### **Clojure/Leiningen Projects**
```dockerfile
# Create a clean artifacts stage for artifact upload  
FROM scratch AS artifacts
COPY --from=builder /app/target/ /artifacts/
```

#### **Node.js Projects**
```dockerfile
# Create a clean artifacts stage for artifact upload
FROM scratch AS artifacts
COPY --from=builder /app/dist/ /artifacts/
```

### 4. Test Locally

Test the artifacts stage builds correctly:

```bash
# Test with Podman
podman build --target artifacts -t my-app-artifacts .

# Verify contents
docker create --name test-extract my-app-artifacts
docker export test-extract | tar -tv | head -20
docker rm test-extract
```

### 5. Commit and Push

```bash
git add .
git commit -m "Enable artifact extraction for CI/CD pipeline"
git push origin enable-artifact-extraction
```

### 6. Create Pull Request

Create a PR to merge the changes. The CI/CD pipeline will now extract artifacts from the container build.

## Pull Request Template

Use this template when creating your PR:

```markdown
## Summary
Enable artifact extraction from container builds for CI/CD pipeline

## Changes
- [ ] Added artifacts stage to Dockerfile/Containerfile
- [ ] Updated GitHub workflow to enable artifact extraction (if needed)
- [ ] Tested artifacts stage builds successfully locally

## Language/Framework
- [ ] SBT/Scala - extracting from `/target/universal/stage/`
- [ ] Clojure/Leiningen - extracting from `/target/`  
- [ ] Node.js - extracting from `/dist/`
- [ ] Other: ________________

## Testing
Tested artifact extraction locally:
```bash
# Replace with your actual test commands
podman build --target artifacts -t [project]-artifacts .
podman create --name test-extract [project]-artifacts
podman export test-extract | tar -tv | grep -E '\.(jar|war|js|css)$'
podman rm test-extract
```

**Artifacts found:** 
- [ ] JAR files (specify size: _____ MB)
- [ ] Distribution files 
- [ ] Other: ________________

## Benefits
- ✅ CI/CD can extract build artifacts without language-specific tooling
- ✅ Faster pipeline execution (no need to install build tools on agents)
- ✅ Consistent artifact collection across different project types

## Breaking Changes
- [ ] None
- [ ] Requires CI/CD pipeline update: ________________

## Additional Notes
_Add any project-specific considerations or dependencies_
```

## Language-Specific Artifact Locations

| **Technology** | **Build Tool** | **Artifact Location** | **Contains** |
|----------------|----------------|----------------------|--------------|
| **Scala/Play** | SBT | `/target/universal/stage/` | Staged application with lib/ and bin/ |
| **Clojure** | Leiningen | `/target/` | Uberjar + regular JAR files |
| **Node.js** | npm/yarn | `/dist/` | Built distribution files |
| **Java/Spring** | Maven | `/target/` | JAR/WAR files |
| **Java/Spring** | Gradle | `/build/libs/` | JAR/WAR files |

## Troubleshooting

- **Build fails**: Check that the source path exists in your builder stage
- **Empty artifacts**: Verify your build process actually generates artifacts in the expected location
- **Wrong path**: Check your project's actual build output directory structure

## Notes

- The `scratch` base image creates a minimal container with only your artifacts
- This approach works with any container runtime (Docker, Podman, etc.)
- CI/CD systems can extract artifacts without language-specific tooling
