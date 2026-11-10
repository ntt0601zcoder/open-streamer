# Template được lưu không kiểm tra input và watermark, failover mất âm thầm

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/domain, internal/manager, internal/coordinator
- **Vị trí:** internal/api/handler/template.go:130-153, internal/api/handler/stream.go:685-693, internal/domain/stream.go:121-148, internal/domain/template.go:247-249, internal/manager/service.go:394-407, internal/coordinator/diff.go:83-90

## Hiện tượng
Template có hai input không ghi `priority` được lưu thành công. Stream kế thừa chỉ thấy một input, nguồn còn lại biến mất và không có failover. Watermark sai trong template cũng chỉ lộ ra khi transcoder chạy.

## Điều kiện kích hoạt
1. `POST /templates/t` với `{"inputs":[{"url":"rtmp://a/x"},{"url":"rtmp://b/y"}]}`.
2. Stream tham chiếu `t` và để trống `inputs`.

## Phân tích
`TemplateHandler.Put` chỉ gọi `ValidatePrefixes` và kiểm tra chồng prefix. Với stream, `decodeStreamBody` chạy `ValidateInputPriorities` và `ValidateUniqueInputs` trên bản thô, nên `inputs` rỗng qua được, còn input từ template thì không ai kiểm tra.

`ResolveStream` gán nguyên danh sách từ template. `Manager.Register` đánh chỉ mục theo priority:

```go
for _, inp := range stream.Inputs {
    state.inputs[inp.Priority] = &InputHealth{
```

Hai phần tử cùng priority 0 chỉ còn một. `diffInputs` cũng dựng map theo priority nên mất tương tự. Handler cũng không gọi `Watermark.Validate`, `validateCopyConfig`, `validateMixerConfig` cho template.

## Vì sao đây là lỗi
Quy tắc "priority phải là 0..N-1" tồn tại chính vì manager và diff dùng priority làm khoá, nhưng template là đường vòng qua quy tắc đó.

## Hướng sửa
Trong `TemplateHandler.Put`, chạy các kiểm tra trên một `Stream` tạm dựng từ template: `ValidateInputPriorities`, `ValidateUniqueInputs`, `Watermark.Validate`. Trả 400 giống đường stream.

## Cách xác minh
Test handler với template có hai input cùng priority, mong đợi 400.
