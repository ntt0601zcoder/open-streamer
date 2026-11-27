# User read-only đọc được secret và tự cấp được token phát

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api, internal/api/handler, internal/domain, config
- **Vị trí:** internal/api/apiauth.go:164-175, internal/api/handler/policy.go:84-113, internal/api/handler/hook.go:69-125, internal/api/handler/config.go:138-180, internal/api/handler/config_yaml.go:59-110, internal/domain/policy.go:65, internal/domain/hook.go:62, config/config.go:280

## Hiện tượng
Một tài khoản chỉ có quyền `read` lấy được `token_secret` của mọi policy, `secret` ký hook, bcrypt hash của tài khoản `write`, và `stream_key`/credential trong input qua các API GET.

## Điều kiện kích hoạt
Auth API bật, đăng nhập user `read`, gọi `GET /policies`, `GET /hooks`, `GET /config` hoặc `GET /config/yaml`.

## Phân tích
Role read cho phép toàn bộ GET:
```go
case http.MethodGet, http.MethodHead, http.MethodOptions:
    return true
```
Các trường bí mật được serialize bình thường:
```go
TokenSecret string `json:"token_secret,omitempty" yaml:"token_secret,omitempty"`
Secret string `json:"secret" yaml:"secret"`
PasswordHash string `... json:"password_hash" yaml:"password_hash"`
```
`writeJSON(... "data": ps ...)` và `yaml.Marshal(full)` không che trường nào.

Với `token_secret`, user read tự ký token bằng `SignToken` để xem mọi stream có policy. Với `secret`, có thể giả mạo webhook. Hash bcrypt cho phép dò mật khẩu offline để nâng lên `write`.

## Vì sao đây là lỗi
Header `apiauth.go` định nghĩa `read` là quyền đọc cấu hình tách khỏi quyền thay đổi. Việc lộ các khoá này cho phép có được quyền lớn hơn role được cấp.

## Hướng sửa
Che các trường bí mật với role read (trả `***` hoặc bỏ hẳn) và khi round-trip YAML thì giữ lại giá trị cũ nếu nhận placeholder. Có thể yêu cầu role write cho `/config/yaml`.

## Cách xác minh
Test middleware + handler với user `read`: `GET /policies` trả JSON có `token_secret`.
