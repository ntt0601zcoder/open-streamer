# POST /hooks chấp nhận ID rỗng và ghi đè hook có sẵn

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/domain, internal/store
- **Vị trí:** internal/api/handler/hook.go:88-107, internal/domain/hook.go (`Hook.Validate`), internal/store/json/store.go:392-397, internal/api/server.go:283, internal/api/handler/config_yaml.go:898-910

## Hiện tượng
Tạo hook không có `id` trả 201 với `id:""`. Hook đó không sửa, xoá hay test được qua `/hooks/{hid}`, và lần tạo tiếp theo không có id sẽ thay thế nó không báo trước. Tạo với id đã tồn tại cũng ghi đè âm thầm.

## Điều kiện kích hoạt
`POST /hooks` với body không có `id` hai lần, hoặc trùng id với hook cũ.

## Phân tích
`Create` chỉ gọi `hook.Validate`, hàm này kiểm tra type và target, rồi lưu:
```go
if err := h.hookRepo.Save(r.Context(), &hook); err != nil {
```
`Save` gán `d.Hooks[string(hook.ID)] = hook`. Route `/{hid}` không khớp với ID rỗng, nên hook đó chỉ xoá được bằng config YAML, mà YAML lại từ chối ID rỗng khi gửi lại.

## Vì sao đây là lỗi
Đường YAML đã có `validateHookID` bắt buộc và kiểm tra trùng. Đường REST là `Create`, và swagger chỉ có 201 cho tạo mới, không phải upsert. Hook có ID rỗng còn làm `PUT /config/yaml` round-trip của chính cấu hình hiện tại thất bại ở bước validate.

## Hướng sửa
Sinh UUID khi `id` trống. Trả 409 khi ID đã tồn tại.

## Cách xác minh
Unit test handler: hai lần `Create` body không có `id`, kiểm tra repo chỉ còn một phần tử với key rỗng.
