#!/bin/bash
# MinIO Build Script
# This script updates dependencies and builds MinIO

set -e  # Exit on error

echo "🔨 Building MinIO..."
echo ""

# Get the directory of this script
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "📦 Step 1: Running go mod tidy..."
go mod tidy
echo "✅ Go dependencies updated"
echo ""

echo "🔨 Step 2: Building MinIO..."
# Check if 'build' target exists, otherwise use default target
if make -n build &>/dev/null; then
    make build
else
    make
fi
echo "✅ MinIO build complete"
echo ""

echo "✅ MinIO build complete!"
echo ""
echo "Built binary: ./minio"
echo ""

# Ask if user wants to push to git
read -p "🚀 Push to git and create RELEASE tag? (y/n): " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    echo ""
    echo "📝 Checking git status..."
    git status --short
    
    # Generate RELEASE tag
    RELEASE_TAG="RELEASE.$(date -u +"%Y-%m-%dT%H-%M-%SZ")"
    echo ""
    echo "🏷️  Generated RELEASE tag: $RELEASE_TAG"
    read -p "   Use this tag? (y/n): " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        echo ""
        echo "📦 Staging changes..."
        # Check if there are any changes before staging
        if [ -n "$(git status --porcelain)" ]; then
            git add go.mod go.sum
            git add -A
            git commit -S -m "Update dependencies and build artifacts"
            echo "✅ Changes committed (signed)"
        else
            echo "ℹ️  No changes to commit"
        fi
        
        echo ""
        echo "🏷️  Creating and pushing tag: $RELEASE_TAG"
        git tag -s "$RELEASE_TAG" -m "Release $RELEASE_TAG"
        git push origin master
        git push origin "$RELEASE_TAG"
        echo "✅ Tag pushed to remote"
        echo ""
        echo "✅ MinIO released with tag: $RELEASE_TAG"
    else
        echo "❌ Tag creation cancelled"
    fi
else
    echo "ℹ️  Skipping git push"
fi
