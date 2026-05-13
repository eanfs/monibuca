#!/bin/bash

# 生成日期版本号：yymmdd 格式（例如 260119 表示 2026-01-19）
DATE_VERSION=`date '+%y%m%d%H%M'`

# 获取最新的 tag
# VERSION=v2.11.5.260119
VERSION=`git describe --tags --abbrev=0`

# 将 . 替换为空格以便分割成数组
VERSION_BITS=(${VERSION//./ })

# 获取版本号各部分并增加修订版本号
VNUM1=${VERSION_BITS[0]}
VNUM2=${VERSION_BITS[1]}
VNUM3=${VERSION_BITS[2]}

# 修订版本号自增
VNUM3=$((VNUM3+1))

# 如果修订版本号超过 999，则次版本号加 1，修订版本号重置为 0
if ((VNUM3 > 999)); then
    VNUM2=$((VNUM2+1))
    VNUM3=0
fi

# 创建新的 tag，格式为 x.xx.xxx.日期
NEW_TAG="$VNUM1.$VNUM2.$VNUM3.$DATE_VERSION"

echo "Updating $VERSION to $NEW_TAG"

# 获取当前 commit hash 并检查是否已有 tag
GIT_COMMIT=`git rev-parse HEAD`
NEEDS_TAG=`git describe --contains $GIT_COMMIT 2>/dev/null`

# 仅在当前 commit 没有 tag 时才创建新 tag
if [ -z "$NEEDS_TAG" ]; then
    git tag $NEW_TAG
    echo "Tagged with $NEW_TAG"
    git push --tags
else
    echo "Already a tag on this commit"
fi