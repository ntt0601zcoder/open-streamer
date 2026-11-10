# Auto-publish không kiểm tra stream cấu hình cùng code

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/autopublish, internal/ingestor/push, internal/coordinator, internal/api/handler
- **Vị trí:** internal/autopublish/service.go:46-51, internal/autopublish/service.go:150-219, internal/coordinator/coordinator.go:314-319, internal/ingestor/push/rtmp_server.go:303-329, internal/api/handler/stream.go:376-386, internal/api/handler/stream.go:715-722, internal/api/handler/config_yaml.go:596-603

## Hiện tượng
Một lệnh push vào đường dẫn trùng code stream cấu hình có thể: bật lại stream operator đã `disabled`; khiến stream cấu hình bị gắn `source: runtime`; làm `DELETE /streams/{code}` trả 204 nhưng không xoá bản ghi, và stream quay lại sau tối đa 10 giây.

## Điều kiện kích hoạt
Template có prefix `region/north` với input `publish://`, và stream cấu hình `region/north/foo` dùng pull input hoặc đang `disabled`.

Tình huống A, stream đang disabled: encoder push vào `region/north/foo`.

Tình huống B, stream pull đang chạy: encoder push vào cùng đường dẫn, sau đó `DELETE /streams/region/north/foo`.

Tình huống C: `PUT /config/yaml` thêm stream cấu hình trùng code stream runtime đang chạy.

## Phân tích
Service chỉ có `templates`, không có stream repo. Registry không có slot cho code này, nên vào `ResolveOrCreate`, dựng stream từ template rồi gọi `coord.Start`.

A: không có gì đang chạy, `startLocked` chỉ xem `Disabled` trên stream vừa dựng (false), nên pipeline chạy bất chấp `disabled: true` trong store.

B: `startLocked` trả nil vì `IsRunning`, entry runtime vẫn được thêm vào:

```go
if err := s.coord.Start(ctx, resolved); err != nil {
...
s.entries[code] = entry
```

Sau đó `registry.Acquire` thất bại và encoder bị từ chối, nhưng entry còn lại. `withStatus` gắn `source: runtime`. `Delete` đi vào nhánh đầu:

```go
if h.autopublish != nil && h.autopublish.StopRuntime(pipelineCtx, code) {
    ...
    w.WriteHeader(http.StatusNoContent)
    return
}
```

Bản ghi không bị xoá và reconciler khởi động lại. Khi nguồn pull im quá 30 giây, reaper cũng dừng stream cấu hình này.

C: `StreamHandler.Put` có guard 409 nhưng đường YAML thì không, `coord.Start` no-op và cấu hình không được áp dụng.

## Vì sao đây là lỗi
Guard chỉ chặn chiều "tạo stream cấu hình trùng runtime" qua REST, còn chiều ngược lại và đường YAML thì không. Push không có key khi template không đặt `stream_key` nên có thể bị bên ngoài kích hoạt.

## Hướng sửa
Inject stream repo vào `autopublish.Service`, từ chối khi code đã có trong store. Trong `upsertStreams`, áp dụng cùng guard runtime như REST.

## Cách xác minh
Test `ResolveOrCreate` với stream repo có code trùng, mong đợi lỗi và không gọi `coord.Start`.
