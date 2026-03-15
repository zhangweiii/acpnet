# Homebrew release notes

This project publishes a Homebrew formula via GoReleaser.

## Required repository variables

Configure these GitHub repository variables before the first tagged release:

- `HOMEBREW_TAP_OWNER`
- `HOMEBREW_TAP_NAME`
- `RELEASE_COMMIT_AUTHOR_NAME`
- `RELEASE_COMMIT_AUTHOR_EMAIL`

## Required repository secret

- `HOMEBREW_TAP_GITHUB_TOKEN`

The token must have permission to push to the target tap repository.

## Result

On a tagged release such as `v0.1.0`, GitHub Actions will:

1. build release archives
2. publish the GitHub Release
3. update the Homebrew formula in the configured tap repository
