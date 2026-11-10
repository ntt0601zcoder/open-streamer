# playback_policy trỏ tới policy không tồn tại vẫn lưu được, toàn bộ người xem bị chặn

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/domain, internal/mediaauth, internal/publisher
- **Vị trí:** internal/api/handler/stream.go:414-420, internal/api/handler/stream.go:682-693, internal/api/handler/template.go:130-153, internal/api/handler/config_yaml.go:825-855, internal/api/handler/config_yaml.go:380-392, internal/mediaauth/mediaauth.go:155-159

## Hiện tượng
Gõ nhầm tên policy trên stream hoặc template đang phát. API trả 200 và mọi người xem HLS, DASH, RTMP, SRT, RTSP lập tức bị từ chối (403). Lý do chỉ xuất hiện ở log mức Info.

## Điều kiện kích hoạt
Stream đang chạy, `POST /streams/{code}` với `{"playback_policy":"pol_typo"}`.

## Phân tích
`Stream.Template` có kiểm tra `TEMPLATE_NOT_FOUND`, còn `PlaybackPolicy` thì không có ở stream, template hay YAML. Diff phát hiện `PlaybackPolicyChanged` và publisher cập nhật ngay, sau đó:

```go
if pol == nil {
    // Stream references a policy that no longer exists — fail closed.
    // The policy delete handler refuses to remove a referenced policy, so
    // this only happens after a direct store edit; denying is the safe call.
    return deny("unknown policy")
}
```

Trong YAML, `applyPolicies` chỉ kiểm tra tham chiếu khi xoá policy cũ, không kiểm tra stream trỏ tới policy chưa từng có.

## Vì sao đây là lỗi
Fail-closed là đúng khi chạy, nhưng giả định trong comment là sai: chỉ cần một request API hợp lệ là đạt trạng thái này. Đây là loại tham chiếu mà `Template` đã được kiểm tra ở lúc lưu.

## Hướng sửa
Inject `PolicyRepository` vào `StreamHandler` và `TemplateHandler`, trả 400 `POLICY_NOT_FOUND` khi không tìm thấy. Trong `validateFullConfig`, kiểm tra tham chiếu theo danh sách policy trong tài liệu.

## Cách xác minh
Test handler với `playback_policy` không tồn tại, mong đợi 400.
