-- 上传状态查询 SQL
-- 用于监控和排查上传问题

-- ============================================
-- 1. 查询上传失败的录像
-- ============================================
SELECT
    id,
    file_name,
    file_path,
    stream_path,
    upload_status,
    upload_error,
    upload_retry,
    duration,
    storage_type,
    created_at,
    end_time
FROM record_streams
WHERE upload_status = 'failed'
ORDER BY created_at DESC
LIMIT 50;

-- ============================================
-- 2. 查询正在上传的录像（可能卡住）
-- ============================================
SELECT
    id,
    file_name,
    file_path,
    upload_status,
    upload_retry,
    created_at,
    TIMESTAMPDIFF(MINUTE, created_at, NOW()) as minutes_elapsed
FROM record_streams
WHERE upload_status = 'uploading'
    AND created_at < DATE_SUB(NOW(), INTERVAL 30 MINUTE)  -- 超过30分钟还在上传
ORDER BY created_at ASC;

-- ============================================
-- 3. 统计上传成功率（按小时）
-- ============================================
SELECT
    DATE_FORMAT(created_at, '%Y-%m-%d %H:00') as hour,
    upload_status,
    COUNT(*) as count,
    AVG(upload_retry) as avg_retry,
    MAX(upload_retry) as max_retry
FROM record_streams
WHERE created_at >= DATE_SUB(NOW(), INTERVAL 24 HOUR)
    AND upload_status IN ('success', 'failed')
GROUP BY hour, upload_status
ORDER BY hour DESC, upload_status;

-- ============================================
-- 4. 查询需要重试次数最多的录像
-- ============================================
SELECT
    id,
    file_name,
    file_path,
    upload_status,
    upload_retry,
    upload_error,
    duration,
    created_at
FROM record_streams
WHERE upload_retry > 0
ORDER BY upload_retry DESC, created_at DESC
LIMIT 20;

-- ============================================
-- 5. 按错误类型统计失败原因
-- ============================================
SELECT
    SUBSTRING_INDEX(upload_error, ':', 1) as error_type,
    COUNT(*) as count,
    AVG(upload_retry) as avg_retry
FROM record_streams
WHERE upload_status = 'failed'
    AND upload_error IS NOT NULL
    AND upload_error != ''
GROUP BY error_type
ORDER BY count DESC;

-- ============================================
-- 6. 查询大文件上传情况（>200MB）
-- ============================================
SELECT
    id,
    file_name,
    duration / 1000 as duration_seconds,
    upload_status,
    upload_retry,
    upload_error,
    created_at
FROM record_streams
WHERE duration > 200000  -- 假设 200 秒 ≈ 200MB
ORDER BY duration DESC, created_at DESC
LIMIT 50;

-- ============================================
-- 7. 查询特定流的上传历史
-- ============================================
-- 使用方法：替换 'your/stream/path' 为实际流路径
SELECT
    id,
    file_name,
    upload_status,
    upload_retry,
    upload_error,
    duration,
    created_at,
    end_time
FROM record_streams
WHERE stream_path = 'your/stream/path'
ORDER BY created_at DESC
LIMIT 100;

-- ============================================
-- 8. 查询今天的上传统计
-- ============================================
SELECT
    upload_status,
    COUNT(*) as count,
    ROUND(COUNT(*) * 100.0 / SUM(COUNT(*)) OVER(), 2) as percentage,
    AVG(upload_retry) as avg_retry,
    AVG(duration) / 1000 as avg_duration_seconds
FROM record_streams
WHERE DATE(created_at) = CURDATE()
GROUP BY upload_status
ORDER BY count DESC;

-- ============================================
-- 9. 查询上传失败但可以手动重试的录像
-- ============================================
-- 这些录像上传失败，但文件可能还在本地
SELECT
    id,
    file_path,
    upload_error,
    upload_retry,
    created_at
FROM record_streams
WHERE upload_status = 'failed'
    AND storage_type = 's3'
    AND created_at >= DATE_SUB(NOW(), INTERVAL 7 DAY)  -- 最近7天
ORDER BY created_at DESC;

-- ============================================
-- 10. 清理测试数据（谨慎使用！）
-- ============================================
-- 删除测试模式的录像记录
-- DELETE FROM record_streams WHERE type = 'test';

-- ============================================
-- 11. 手动更新上传状态（紧急修复）
-- ============================================
-- 将卡住的上传状态重置为 pending，以便重新上传
-- UPDATE record_streams
-- SET upload_status = 'pending', upload_retry = 0, upload_error = NULL
-- WHERE upload_status = 'uploading'
--     AND created_at < DATE_SUB(NOW(), INTERVAL 1 HOUR);

-- ============================================
-- 12. 创建监控视图（可选）
-- ============================================
CREATE OR REPLACE VIEW v_upload_monitor AS
SELECT
    DATE_FORMAT(created_at, '%Y-%m-%d %H:00') as hour,
    upload_status,
    COUNT(*) as count,
    AVG(upload_retry) as avg_retry,
    SUM(CASE WHEN upload_retry > 0 THEN 1 ELSE 0 END) as retry_count,
    AVG(duration) / 1000 as avg_duration_seconds
FROM record_streams
WHERE created_at >= DATE_SUB(NOW(), INTERVAL 24 HOUR)
GROUP BY hour, upload_status;

-- 使用视图
-- SELECT * FROM v_upload_monitor ORDER BY hour DESC;

-- ============================================
-- 13. 性能优化：添加索引（如果需要）
-- ============================================
-- 为上传状态查询添加索引
-- CREATE INDEX idx_upload_status ON record_streams(upload_status, created_at);
-- CREATE INDEX idx_stream_path_created ON record_streams(stream_path, created_at);
-- CREATE INDEX idx_created_at ON record_streams(created_at);
