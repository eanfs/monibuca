#!/bin/sh
# 获取最新的 tag
LATEST_TAG=$(git describe --tags $(git rev-list --tags --max-count=1))

if [ -z "$LATEST_TAG" ]; then
  echo "没有找到任何 tag."
  exit 1
fi

echo "最新的 tag 是: $LATEST_TAG"

# 删除 tag
git tag -d $LATEST_TAG
git push --delete origin $LATEST_TAG


git tag $LATEST_TAG
git push --tag