---
description: Upgrade this project to use the new unified build github actions workflow
agent: "build"
subtask: false
---

This repo has been selected for upgrade to a new build process. please create a branch called `migrate-to-unified-workflow`.
Follow the upgrade plan 
!`cat /Users/ben/code/github-actions/docs/migration-to-build-and-release-container-image.md`

NOTE: when testing on this machine, use podman, and make sure to start it first using `podman machine start`
For testing a multi-stage Docker build with proper args and timeout:
```sh
podman build -t test-image \
  --build-arg VERSION=test \
  --build-arg GIT_SHA=local \
  --build-arg BUILD_DATE=$(date -Iminutes) \
  .
```
Then verify it worked:

`podman images | grep test-image`

Once you are done, use the template below to describe the changes (taken from another repo that has already done this, but make sure to update it for relevance to this pr), you can use the same ticket number.
Make the pr against the main branch.

## Summary

**CLOUD-1259: Migrate search-gateway to unified multistage Docker build workflow**

This PR migrates the search-gateway service from an "in-runner" build process (where artifacts are built in CI and copied into Docker) to a standardized multistage Docker build. The service builds successfully, but **requires developer testing before production deployment** to validate runtime behavior.

## Key Changes

### Build Process Migration
- **Before**: JAR built in CI runner → copied into single-stage Dockerfile  
- **After**: Multistage Docker build with dedicated builder stage using `clojure:temurin-11-lein-2.12.0`
- Tests now run during Docker build (`lein test`) - build fails if tests fail
- Dependency caching optimized with Maven cache mounts

### Workflow Consolidation  
- Replaced custom build workflows with shared `build-release-container-image.yml@v1`
- Simplified CI logic by removing duplicate path filtering and version bump handling
- Consolidated `schedule-build.yml` into `test-build.yml`

### Runtime Environment
- Removed unused certificate handling (FNZSL certs were never actually utilized)
- Enhanced security with explicit service user permissions (`USER service`)
- Improved container metadata with Datadog and OCI labels

## Testing Request

**⚠️ Developer validation required before production deployment**

Since this changes the fundamental build process, please:

1. **Deploy alongside your next regular release** to dev environment
2. **Verify application startup** and basic functionality 
3. **Test service-to-service communication** with oneview/onestore dependencies
4. **Confirm logging and monitoring** work as expected
5. **Validate any environment-specific behaviors**

The build artifacts should be functionally identical, but runtime validation ensures no edge cases were missed during the migration.

## Next Steps

This PR is ready to merge. The application development team should take ownership from here to ensure safe deployment and testing outside of business hours, following your standard release procedures and rollback protocols.
