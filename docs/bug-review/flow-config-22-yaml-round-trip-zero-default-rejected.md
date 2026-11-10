# GET rồi PUT /config/yaml nguyên văn có thể bị từ chối 422

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/buffer, cmd/server, config
- **Vị trí:** internal/api/handler/config_yaml.go:715-727, internal/buffer/service.go:131-133, cmd/server/main.go:264-267, config/config.go (HooksConfig.WorkerCount), internal/api/handler/config.go:213-229

## Hiện tượng
Cấu hình có `buffer.capacity: 0` hoặc `hooks.worker_count: 0`, đều hợp lệ khi chạy và được lưu qua `POST /config`, mở trong trình soạn YAML rồi bấm lưu mà không sửa gì sẽ bị 422 "must be > 0".

## Điều kiện kích hoạt
1. `POST /config` với `{"hooks":{"file_root_dir":"/x"}}` khi mục `hooks` chưa có, nên `worker_count` là 0.
2. `GET /config/yaml`, rồi `PUT` đúng nội dung đó.

## Phân tích
Validator YAML:

```go
if c.Buffer.Capacity <= 0 { ... "must be > 0" }
if c.Hooks.WorkerCount <= 0 { ... "must be > 0" }
```

Trong khi runtime coi 0 là mặc định: `buffer.New` đặt `DefaultBufferCapacity`, `wireServices` đặt 4 worker, và comment của `WorkerCount` ghi "0 = use 4".

## Vì sao đây là lỗi
Một giá trị được server tự xuất ra lại bị chính server từ chối, trái với mục đích round-trip được nêu trong `GetConfigYAML`. `/config/defaults` cũng quảng bá capacity mặc định khi để trống.

## Hướng sửa
Đổi validator thành `< 0` cho hai trường này.

## Cách xác minh
Test `validateGlobalConfig` với `Buffer{Capacity:0}` và `Hooks{WorkerCount:0}`, mong đợi không có lỗi.
