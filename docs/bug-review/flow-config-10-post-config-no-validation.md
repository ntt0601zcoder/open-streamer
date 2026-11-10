# POST /config không validate, cấu hình sai làm mất API và tồn tại qua restart

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/runtime, internal/api, internal/store
- **Vị trí:** internal/api/handler/config.go:213-229, internal/api/handler/config_probe.go:50-52, internal/api/handler/config_yaml.go:706-735, internal/runtime/manager.go:110-122, internal/runtime/manager.go:233-237, internal/api/server.go:446-448

## Hiện tượng
Một lần `POST /config` với `server.http_addr` sai làm API và cả trang media HTTP ngừng hoạt động. Giá trị sai đã được ghi vào store nên restart process cũng không khôi phục, operator phải sửa file dữ liệu bằng tay.

## Điều kiện kích hoạt
`POST /config` với `{"server":{"http_addr":"8080"}}` (thiếu dấu hai chấm). `PUT /config/yaml` cùng giá trị sẽ bị từ chối 422.

## Phân tích
Luồng POST chỉ gọi một hàm rỗng:

```go
func (h *ConfigHandler) validateTranscoderPath(_ context.Context, _ any) *putValidationError {
    return nil
}
```

Toàn bộ `validateGlobalConfig` chỉ chạy ở đường YAML. `Manager.Apply` lưu trước rồi diff: `diffService("http", ...)` dừng server cũ và khởi động server mới, `ListenAndServe` thất bại, `startService` chỉ log Warn. Không có rollback. Lúc boot, `applyAll` lại dùng đúng giá trị đó.

Cùng lỗ hổng cho các cấu hình khác: `publisher.hls.dir == publisher.dash.dir` (sau restart, `Publisher.Start` lỗi với mọi stream bật cả hai và reconciler thất bại lặp lại), port ngoài khoảng, `log.level` sai, CORS `*` kèm `allow_credentials`.

## Vì sao đây là lỗi
Hai đường ghi vào cùng một cấu hình có mức kiểm tra khác nhau. Handler trả về lỗi 200 thành công ngay trước khi API biến mất.

## Hướng sửa
Gọi `validateGlobalConfig(&merged)` trong `UpdateConfig` và trả 422 giống YAML. Với `server.*`, cân nhắc thử bind trước khi lưu hoặc rollback khi server mới không lên.

## Cách xác minh
Test handler với `http_addr` không hợp lệ, mong đợi 4xx và `rtm.Apply` không được gọi. Chỉ thử trên instance riêng, không chạy trên server đang phát.
