# PUT /config/yaml bỏ qua các kiểm tra mà REST API bắt buộc

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/domain, internal/autopublish, internal/ingestor, internal/transcoder
- **Vị trí:** internal/api/handler/config_yaml.go:627-647, internal/api/handler/config_yaml.go:825-855, internal/api/handler/config_yaml.go:316-322, internal/api/handler/config_yaml.go:177-197, internal/api/handler/stream.go:394-420, internal/api/handler/stream.go:691-693, internal/api/handler/template.go:137-153, internal/domain/copy_shape.go:101-106, docs/ARCHITECTURE.md:1060-1061

## Hiện tượng
Tài liệu YAML được chấp nhận dù có cấu hình mà REST từ chối: stream tự `copy://` chính nó, stream trỏ template không tồn tại, watermark sai, prefix trùng hoặc chồng nhau giữa các template, template không có `code`. Các lỗi chỉ lộ ra khi chạy. Ngoài ra lỗi tham chiếu template hoặc policy bị phát hiện sau khi GlobalConfig đã được áp dụng, để lại trạng thái áp dụng dở dang.

## Điều kiện kích hoạt
`PUT /config/yaml` có:

```yaml
streams:
  - code: a
    inputs: [{url: "copy://a", priority: 0}]
templates:
  - {code: t1, prefixes: ["live"]}
  - {code: t2, prefixes: ["live/x"]}
```

## Phân tích
`validateFullConfig` không có nhánh cho `Templates`. `validateStreams` chỉ kiểm tra code, priority và URL trùng, thiếu các bước của `StreamHandler.Put`: `validateCopyConfig`, `validateMixerConfig`, `TEMPLATE_NOT_FOUND`, `Watermark.Validate`.

Hệ quả cụ thể:
- Stream trỏ template thiếu: `resolveStream` trả bản thô, không có input nên không bao giờ chạy và không có lỗi.
- `copy://a` trong chính `a`: CopyReader tự đăng ký buffer của mình, lỗi mà `ValidateCopyShape` được viết để chặn.
- Prefix chồng nhau: phá vỡ invariant trong ARCHITECTURE "Template prefixes are globally non-overlapping".

Thứ tự áp dụng cũng có vấn đề: `applyTemplates` và `applyPolicies` trả lỗi "referenced by stream(s)" sau khi `rtm.Apply` đã chạy.

## Vì sao đây là lỗi
Comment nói "nothing is applied until every section passes", nhưng vòng validate không bao phủ những điều kiện này.

## Hướng sửa
Tách logic validate của REST thành hàm dùng chung, chạy trong `validateFullConfig` trên trạng thái sau khi thay thế, bao gồm kiểm tra tham chiếu template và policy.

## Cách xác minh
Test `validateFullConfig` với từng ví dụ trên, mong đợi có `fieldError`.
