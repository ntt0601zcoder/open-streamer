# Bug review — tổng hợp

210 file từ 9 reviewer, gộp thành **175 lỗi riêng biệt**. Tất cả là **nghi ngờ từ đọc code**, chưa build, chạy hay tái hiện.

| Mức độ | Số lỗi |
|---|---|
| Nghiêm trọng | 9 |
| Cao | 50 |
| Trung bình | 65 |
| Thấp | 51 |

Cột *Trùng với* liệt kê các file khác mô tả cùng lỗi. 🔁 đánh dấu lỗi có thể xuất hiện trên các stream soak đang chạy.

## Nghiêm trọng

| | Lỗi | Tin cậy | File | Trùng với |
|---|---|---|---|---|
| 🔁 | copy:// và mixer:// từ upstream có transcoder không nhận được packet nào | Cao | [core-01](core-01-streammainbufferists-transcoder-blackout.md) | [flow-lifecycle-21](flow-lifecycle-21-ts-readers-starve-on-av-renditions.md), [publisher-01](publisher-01-mpegts-drops-av-packets.md) |
|  | Supervisor treo vĩnh viễn khi subprocess transcoder chết | Cao | [flow-lifecycle-01](flow-lifecycle-01-supervisor-waitch-deadlock.md) |  |
|  | Đổi cấu hình listener RTSP làm cả server panic | Cao | [flow-lifecycle-02](flow-lifecycle-02-rtsp-listener-reload-panic.md) |  |
|  | Slot push không bao giờ được gỡ, stream auto-publish đã reap không thể tạo lại | Cao | [flow-lifecycle-03](flow-lifecycle-03-push-registry-never-unregistered.md) | [ingest-01](ingest-01-autopublish-stale-registry.md) |
|  | Stream auto-publish có transcoder bị reaper giết sau 30 giây dù encoder vẫn đẩy | Cao | [flow-lifecycle-04](flow-lifecycle-04-autopublish-liveness-transcoded.md) |  |
|  | Pusher và pull backup cùng ghi vào một buffer, pull worker không dừng sau failback | Cao | [flow-lifecycle-05](flow-lifecycle-05-failback-publish-keeps-pull-worker.md) | [ingest-02](ingest-02-push-failback-keeps-pull-worker.md) |
|  | DVR suy wallclock từ PTS, PTS reset làm xoá các giờ đã ghi | Cao | [flow-media-01](flow-media-01-dvr-wall-from-pts-deletes-hours.md) |  |
|  | Probe tính mỗi segment HLS là một packet nên input HLS gần như không thể phục hồi | Cao | [ingest-03](ingest-03-hls-probe-never-sustained.md) |  |
|  | Một request HTTP có path không phải UTF-8 làm sập cả server sau khi hết idle | Cao | [services-01](services-01-sessions-invalid-utf8-crash.md) |  |

## Cao

| | Lỗi | Tin cậy | File | Trùng với |
|---|---|---|---|---|
|  | Thêm input dự phòng khi stream đã exhausted không có tác dụng | Cao | [control-02](control-02-added-input-ignored-when-exhausted.md) |  |
|  | Đổi StreamKey trên stream đang chạy không có hiệu lực | Cao | [control-05](control-05-streamkey-change-not-applied.md) | [flow-lifecycle-17](flow-lifecycle-17-streamkey-not-hot-reloaded.md), [flow-config-02](flow-config-02-stream-key-not-diffed.md) |
|  | Joint seed bỏ qua SameTimebaseMaxMs, mixer:// bị lệch A/V cố định theo chênh lệch uptime | Cao | [core-02](core-02-joint-seed-no-timebase-guard.md) |  |
|  | Sau reconnect, joint seed đưa output DTS về 0 rồi nhảy về wallclock trong cùng session | Cao | [core-03](core-03-joint-seed-resets-output-to-zero.md) |  |
| 🔁 | Re-anchor do input lùi đẩy track còn lại đi xa, timeline vượt wallclock vĩnh viễn | Cao | [core-04](core-04-reanchor-partner-shift-on-regress.md) |  |
|  | Đường raw-TS loại bỏ MP2/MP3/AC-3 và MPEG-2 video, kênh DVB mất tiếng hoặc mất hẳn output | Cao | [core-05](core-05-tsnorm-drops-non-aac-audio.md) | [ingest-07](ingest-07-tsnorm-drops-dvb-codecs.md) |
|  | Sửa template qua PUT /config/yaml không đến được stream đang chạy | Cao | [flow-config-01](flow-config-01-yaml-template-change-no-reload.md) | [control-08](control-08-yaml-template-change-no-reload.md) |
|  | Cập nhật template không đến được stream auto-publish đang chạy | Cao | [flow-config-03](flow-config-03-template-update-skips-runtime-streams.md) | [flow-lifecycle-19](flow-lifecycle-19-template-update-skips-runtime-streams.md), [control-09](control-09-template-update-skips-runtime-streams.md) |
|  | POST stream giữ lại trường của phần tử mảng cũ, có thể gửi nhầm credential sang input khác | Cao | [flow-config-06](flow-config-06-json-merge-slice-element-leak.md) |  |
|  | Hook không bao giờ nhận được nhiều loại event đã được công bố | Cao | [flow-config-07](flow-config-07-hooks-missing-event-subscriptions.md) |  |
|  | Sửa hook HTTP vẫn gửi đến URL cũ với secret cũ | Cao | [flow-config-08](flow-config-08-hook-batcher-stale-target-secret.md) |  |
|  | POST /config không validate, cấu hình sai làm mất API và tồn tại qua restart | Cao | [flow-config-10](flow-config-10-post-config-no-validation.md) |  |
|  | Bản ghi DVR thành công khai khi stream runtime kết thúc hoặc stream bị xoá | Cao | [flow-config-14](flow-config-14-dvr-archive-public-when-stream-gone.md) |  |
|  | Prefix auto-publish "live" như tài liệu không bao giờ khớp | Cao | [flow-config-30](flow-config-30-autopublish-live-prefix-never-matches.md) | [core-17](core-17-live-prefix-code-collision.md) |
|  | Reload transcoder với encoder đang push làm output đen hoặc failover giả | Cao | [flow-lifecycle-06](flow-lifecycle-06-reload-stale-pusher-buffer.md) | [ingest-04](ingest-04-publisher-not-kicked-on-reload.md) |
|  | Restart listener RTMP làm mất play handler, auto-publish, xác thực push và callback | Cao | [flow-lifecycle-07](flow-lifecycle-07-rtmp-restart-loses-handlers.md) |  |
|  | Update không kiểm tra lại trạng thái chạy, PUT trong lúc Stop tạo pipeline mồ côi | Cao | [flow-lifecycle-08](flow-lifecycle-08-update-no-running-recheck.md) |  |
|  | Failover sang input lỗi đồng bộ làm stream kẹt vĩnh viễn mà không báo Degraded | Cao | [flow-lifecycle-12](flow-lifecycle-12-failover-start-error-wedges.md) | [control-01](control-01-failover-start-error-stuck.md), [ingest-10](ingest-10-failover-start-error-stuck.md) |
| 🔁 | Transcoder respawn đưa PTS rendition về gần 0 mà không có SessionStart | Cao | [flow-media-02](flow-media-02-transcoder-respawn-no-session.md) |  |
|  | Scanner HLS theo PID H.264 ảo nên nguồn raw-TS HEVC không cắt được tại IDR | Cao | [flow-media-03](flow-media-03-hls-scanner-follows-phantom-h264.md) | [core-06](core-06-tsnorm-hevc-phantom-h264-pcr.md), [ingest-16](ingest-16-tsnorm-h265-psi.md) |
|  | RTMP play không xử lý ranh giới session, timestamp tràn số sau failover | Cao | [flow-media-04](flow-media-04-rtmp-play-timestamp-underflow.md) |  |
|  | RTSP gọi DTSExtractor trước khi dùng SPS từ SDP, nguồn chỉ khai báo tham số trong SDP mất toàn bộ hình | Cao | [ingest-05](ingest-05-rtsp-dts-before-param-cache.md) |  |
|  | Buffer UDP mặc định 1316 byte cắt cụt datagram RTP/MPEG-TS | Cao | [ingest-06](ingest-06-udp-rtp-truncated.md) |  |
|  | file:// .ts đọc với tốc độ đĩa, không có pacing theo thời gian thực | Cao | [ingest-08](ingest-08-ts-file-unpaced.md) |  |
|  | HLS reader bỏ qua mọi segment khi EXT-X-MEDIA-SEQUENCE của upstream reset | Cao | [ingest-09](ingest-09-hls-media-sequence-reset.md) |  |
|  | tsnorm gộp mọi track cùng codec vào một PID, feed đa ngôn ngữ bị trộn audio | Cao | [ingest-12](ingest-12-tsnorm-merges-audio-tracks.md) |  |
| 🔁 | RTSP làm sai timestamp của B-frame do kẹp đơn điệu trên PTS | Cao | [publisher-02](publisher-02-rtsp-bframe-timestamp-clamp.md) |  |
| 🔁 | RTP timestamp RTSP hỏng khi tràn uint32 trên stream chạy dài (video ~13,25 giờ, audio ~24,85 giờ) | Cao | [publisher-03](publisher-03-rtsp-rtp-timestamp-wrap.md) |  |
|  | RTMP push gửi nguyên PES AAC nhiều frame thành một tag, làm hỏng audio | Cao | [publisher-04](publisher-04-push-aac-bundle-not-split.md) |  |
|  | DASH đóng băng live edge khi audio mất giữa chừng do còn sót frame trong queue | Cao | [publisher-10](publisher-10-dash-audio-death-freezes-cuts.md) |  |
|  | DASH cắt mất IDR đầu tiên khi ghép cặp V/A, segment đầu bắt đầu bằng P/B | Cao | [publisher-11](publisher-11-dash-pairing-truncate-drops-idr.md) |  |
|  | DASH giữ init segment cũ khi failover sang nguồn khác tham số codec | Cao | [publisher-13](publisher-13-dash-init-stale-after-session.md) |  |
| 🔁 | tfdt DASH nối tiếp không bao giờ neo lại sau gián đoạn, timeline tụt khỏi đồng hồ và lệch V/A khi track quay lại | Cao | [publisher-14](publisher-14-dash-sequential-tfdt-never-reanchors.md) |  |
| 🔁 | DASH luôn khai AAC stereo, sai với nguồn mono hoặc 5.1 | Cao | [publisher-18](publisher-18-dash-aac-channel-config-hardcoded.md) |  |
| 🔁 | HLS master khai `avc1` cho rendition H.265 | Cao | [publisher-22](publisher-22-hls-master-h265-declared-avc1.md) |  |
|  | HLS master bỏ audio khi `audio.codec` để trống dù transcoder vẫn encode AAC | Cao | [publisher-23](publisher-23-hls-abr-hasaudio-empty-codec.md) | [flow-media-09](flow-media-09-hls-master-audio-zero-config.md) |
|  | RTSP định tuyến theo tiền tố nên có thể phát nhầm stream và lách playback policy | Cao | [publisher-29](publisher-29-rtsp-mount-prefix-wrong-stream.md) |  |
| 🔁 | Client HTTP MPEG-TS bị cắt đúng sau 120 giây | Cao | [services-02](services-02-mpegts-120s-timeout.md) |  |
|  | File tạm multipart của upload không bao giờ bị xoá, mỗi upload VOD làm đầy /tmp thêm một lần | Cao | [services-04](services-04-multipart-temp-file-leak.md) |  |
| 🔁 | Request media không tồn tại tạo session, event và series Prometheus không giới hạn | Cao | [services-07](services-07-sessions-unvalidated-path-cardinality.md) |  |
|  | Hour DVR dở dang sau khi process chết nằm lại catalog mãi và làm hỏng retention theo dung lượng | Cao | [services-09](services-09-dvr-unsealed-hour-never-pruned.md) |  |
|  | Restart stream giữa giờ xoá toàn bộ phần đã ghi của giờ UTC đó | Cao | [services-10](services-10-dvr-midhour-restart-discards-hour.md) |  |
|  | Sau restart, mọi đoạn DVR của lần chạy trước không phát được dù status vẫn báo có | Cao | [services-11](services-11-dvr-restart-origin-breaks-old-archive.md) |  |
|  | Mất audio giữa chừng làm DVR ngừng cắt fragment vĩnh viễn | Cao | [services-12](services-12-dvr-audio-stall-freezes-recording.md) |  |
| 🔁 | Segment HLS lớn hơn 4 MiB làm gRPC server từ chối và transcoder crash-loop | Cao | [transcoder-01](transcoder-01-grpc-4mib-hls-segment.md) |  |
|  | Failover từ input AV-path sang input raw-TS không probe lại định dạng | Cao | [transcoder-02](transcoder-02-av-to-ts-switch-no-reprobe.md) |  |
|  | audio.channels lớn hơn 2 làm subprocess SIGSEGV ở AudioFifo | Cao | [transcoder-03](transcoder-03-audio-multichannel-null-plane.md) |  |
|  | Backend NVENC với encoder không phải NVENC vẫn đi đường GPU và nhận frame CUDA | Cao | [transcoder-04](transcoder-04-gpu-path-non-nvenc-encoder.md) |  |
|  | Audio codec không qua ResolveAudioEncoder: mp2a/mp3 crash-loop, ac3/eac3 bị gói thành ADTS | Cao | [transcoder-05](transcoder-05-audio-codec-not-resolved.md) |  |
|  | Graph GPU có watermark ảnh phải chạy đuổi từ 0 tới PTS nguồn, gây đứng pipeline lâu | Trung bình | [transcoder-08](transcoder-08-gpu-image-watermark-catchup.md) |  |

## Trung bình

| | Lỗi | Tin cậy | File | Trùng với |
|---|---|---|---|---|
|  | Failover sang input publish:// chưa có encoder làm stream kẹt Degraded vĩnh viễn | Cao | [control-03](control-03-push-failover-leaves-exhausted-flag.md) |  |
|  | Diff input bỏ sót Program và Pids, đổi cấu hình lọc UDP không được áp dụng | Cao | [control-06](control-06-inputs-equal-misses-program-pids.md) | [flow-lifecycle-18](flow-lifecycle-18-input-program-pids-not-diffed.md), [flow-config-04](flow-config-04-input-program-pids-not-diffed.md) |
|  | Đổi timeout hoặc limit của push destination đang chạy không có hiệu lực | Cao | [control-07](control-07-push-params-change-not-applied.md) | [flow-lifecycle-23](flow-lifecycle-23-push-destination-settings-ignored.md), [flow-config-05](flow-config-05-push-dest-settings-not-applied.md) |
|  | Xoá input đang active khi không còn candidate, worker cũ vẫn ghi vào stream | Cao | [control-10](control-10-remove-active-input-keeps-worker.md) | [flow-config-19](flow-config-19-remove-last-input-worker-continues.md) |
|  | DVR bị bỏ qua không báo lỗi trên stream ABR copy và ABR mixer | Cao | [control-11](control-11-abr-copy-mixer-dvr-ignored.md) | [flow-config-16](flow-config-16-abr-copy-mixer-dvr-never-started.md) |
|  | POST /config chấp nhận buffer, publisher, ingestor, watermarks nhưng không áp dụng | Cao | [control-13](control-13-config-sections-not-hot-applied.md) | [flow-config-09](flow-config-09-global-sections-not-hot-applied.md) |
|  | Dạng buffer chỉ suy ra từ Inputs[0], failover sang input khác kiểu làm copy:// và mixer:// mất dữ liệu | Cao | [core-07](core-07-input0-only-buffer-shape.md) | [flow-lifecycle-22](flow-lifecycle-22-main-buffer-shape-first-input-only.md), [ingest-14](ingest-14-copy-mixer-fixed-shape.md) |
|  | Marker SessionStart một lần bị mất với subscriber có channel đầy | Trung bình | [core-08](core-08-sessionstart-lost-on-full-channel.md) |  |
|  | Watermark position=custom và resize được chấp nhận nhưng bị bỏ qua | Cao | [core-15](core-15-watermark-custom-position-ignored.md) |  |
|  | PUT /config/yaml bỏ qua các kiểm tra mà REST API bắt buộc | Cao | [flow-config-11](flow-config-11-yaml-validation-gaps.md) |  |
|  | Template được lưu không kiểm tra input và watermark, failover mất âm thầm | Cao | [flow-config-12](flow-config-12-template-put-no-input-validation.md) |  |
|  | protocols.rtmp và protocols.srt không có tác dụng, mọi stream luôn phát được qua RTMP và SRT | Cao | [flow-config-13](flow-config-13-protocols-rtmp-srt-ignored.md) |  |
|  | Mọi lần lưu đều dựng lại toàn bộ pipeline ABR copy và ABR mixer | Cao | [flow-config-17](flow-config-17-abr-copy-mixer-cycle-on-noop-update.md) |  |
|  | PUT /config/yaml không phát các event chi tiết như tài liệu mô tả | Cao | [flow-config-20](flow-config-20-yaml-no-fine-grained-events.md) |  |
|  | playback_policy trỏ tới policy không tồn tại vẫn lưu được, toàn bộ người xem bị chặn | Cao | [flow-config-24](flow-config-24-playback-policy-unvalidated.md) |  |
|  | copy:// và mixer:// không dùng được với upstream là stream auto-publish | Cao | [flow-config-27](flow-config-27-copy-runtime-upstream-not-found.md) |  |
|  | VAAPI và QSV được API quảng bá và chấp nhận nhưng pipeline không có đường chạy | Trung bình | [flow-config-29](flow-config-29-vaapi-qsv-advertised.md) |  |
|  | Xoá watermark đang được dùng không bị chặn, stream mất logo âm thầm | Trung bình | [flow-config-31](flow-config-31-watermark-delete-no-reference-check.md) |  |
|  | Start từ snapshot cũ hồi sinh stream đã xoá hoặc đã disable | Cao | [flow-lifecycle-09](flow-lifecycle-09-stale-start-resurrects-stream.md) | [flow-config-25](flow-config-25-reconciler-stale-snapshot.md) |
|  | Start xoá cờ exhausted vừa được đặt, stream chết vẫn báo Active | Trung bình | [flow-lifecycle-10](flow-lifecycle-10-cleardegradation-after-register.md) | [control-04](control-04-start-clears-fresh-exhausted.md) |
|  | Cờ transcoder unhealthy kẹt sau một lần crash nhanh dù subprocess đã ổn định | Cao | [flow-lifecycle-11](flow-lifecycle-11-transcoder-unhealthy-sticks.md) |  |
|  | Push vào code của stream cấu hình pull làm auto-publish chiếm stream đó | Cao | [flow-lifecycle-13](flow-lifecycle-13-autopublish-claims-config-stream.md) | [flow-config-15](flow-config-15-autopublish-collides-config-stream.md) |
|  | Reload từ passthrough sang transcoder bỏ rơi subscriber của buffer chính | Cao | [flow-lifecycle-14](flow-lifecycle-14-reload-orphans-main-buffer-subscribers.md) |  |
|  | Shutdown không dừng pipeline, để lại subprocess mồ côi và DVR chưa seal | Cao | [flow-lifecycle-15](flow-lifecycle-15-shutdown-no-pipeline-teardown.md) | [control-14](control-14-shutdown-does-not-stop-pipelines.md) |
|  | Frame của session cũ lọt vào queue DASH sau boundary, làm kẹt việc cắt segment | Trung bình | [flow-media-07](flow-media-07-dash-old-session-frame-poisons-queue.md) |  |
|  | Nguồn chỉ có audio đi qua transcoder không tạo ra output | Cao | [flow-media-08](flow-media-08-transcoder-audio-only-gated.md) |  |
|  | Worker cũ ghi sau SetSession của worker mới và chiếm cờ SessionStart | Trung bình | [ingest-11](ingest-11-session-start-stolen-by-old-worker.md) |  |
|  | Rớt kết nối RTMP, RTSP, SRT bị coi là EOF nên worker dừng thay vì reconnect | Cao | [ingest-13](ingest-13-eof-stops-worker-no-reconnect.md) |  |
|  | RTMP play không gửi lại sequence header khi nguồn đổi tham số codec | Cao | [ingest-15](ingest-15-rtmp-play-stale-seq-header.md) | [publisher-30](publisher-30-rtmp-play-stale-seq-header.md) |
|  | HLS pull bắt đầu từ segment cũ nhất và đẩy dồn cả playlist không pacing | Cao | [ingest-17](ingest-17-hls-join-oldest-burst.md) |  |
|  | tsnorm không thực sự resync khi lệch căn 188 byte; .m2ts và HTTP-TS lệch biên hỏng vĩnh viễn | Cao | [ingest-18](ingest-18-tsnorm-no-resync.md) |  |
|  | Allowlist pids không có PMT cho ra stream rỗng, trái với mục đích xử lý PSI lỗi | Cao | [ingest-19](ingest-19-pidfilter-no-psi-empty.md) |  |
|  | Mixer mất trạng thái ABR khi Open thất bại một phần nên lần retry trả EOF giả | Cao | [ingest-20](ingest-20-mixer-partial-open.md) |  |
|  | RTMP push tự ngắt phiên vừa kết nối khi gặp SessionStart đầu tiên | Cao | [publisher-05](publisher-05-push-first-sessionstart-teardown.md) |  |
|  | RTMP push gửi inter-frame trước AVC sequence header sau mỗi lần kết nối | Cao | [publisher-06](publisher-06-push-inter-frames-before-seq-header.md) |  |
|  | Trạng thái push `failed` bị xóa ngay nên API và metric không bao giờ thấy | Cao | [publisher-07](publisher-07-push-failed-status-removed.md) |  |
|  | Bộ đếm retry của push không reset sau phiên thành công | Trung bình | [publisher-08](publisher-08-push-limit-never-reset.md) |  |
|  | DASH không chờ IDR sau session boundary, segment sau failover bắt đầu giữa GOP | Cao | [publisher-12](publisher-12-dash-firstidr-not-rearmed-on-session.md) |  |
|  | Mỗi session boundary tích lũy thêm độ lệch A/V trong DASH do không ghép cặp lại | Trung bình | [publisher-15](publisher-15-dash-session-boundary-av-offset.md) | [flow-media-06](flow-media-06-dash-session-no-repairing.md) |
|  | Safety-net cut của DASH làm audio tụt dần so với video trên nguồn GOP dài | Cao | [publisher-16](publisher-16-dash-safety-net-av-desync.md) |  |
|  | DASH thiếu bù composition offset, video có B-frame hiển thị trễ hơn audio | Trung bình | [publisher-17](publisher-17-dash-bframe-cto-av-offset.md) |  |
|  | Một lỗi parse TS làm output DASH hoặc RTSP raw-TS dừng tới khi restart stream | Trung bình | [publisher-21](publisher-21-demux-error-kills-dash-rtsp-output.md) |  |
|  | HLS audio-only trên đường AV: segment dài 4 lần và mỗi segment có DISCONTINUITY | Cao | [publisher-24](publisher-24-hls-audio-only-force-flush.md) |  |
|  | EXTINF của HLS raw-TS tính theo thời gian thực, không khớp nội dung khi nguồn đến theo đợt | Trung bình | [publisher-27](publisher-27-hls-raw-ts-extinf-wallclock.md) | [flow-media-10](flow-media-10-hls-rawts-extinf-wallclock.md) |
|  | Dừng stream cha xóa luôn thư mục HLS/DASH của stream con | Cao | [publisher-28](publisher-28-nested-stream-code-removeall.md) |  |
|  | RTMP play dùng hai mốc thời gian riêng cho audio và video, tạo lệch A/V cố định | Cao | [publisher-31](publisher-31-rtmp-play-separate-av-base.md) | [flow-media-05](flow-media-05-rtmp-play-separate-av-base.md) |
| 🔁 | H.265 bị bỏ âm thầm ở RTMP push, RTMP play và RTSP trong khi trạng thái báo bình thường | Cao | [publisher-32](publisher-32-h265-silent-rtmp-rtsp-push.md) |  |
|  | `DELETE /sessions/{id}` không ngắt kết nối RTMP, SRT, RTSP, HTTP-TS | Cao | [publisher-34](publisher-34-kick-does-not-disconnect.md) |  |
|  | Client HTTP-TS ngừng đọc giữ goroutine, subscriber và slot giới hạn vô thời hạn | Trung bình | [publisher-35](publisher-35-mpegts-stalled-client-holds-slot.md) |  |
|  | Retention xoá cả hour đang ghi của lane khác | Trung bình | [services-13](services-13-dvr-retention-deletes-open-hour.md) |  |
|  | Retention chỉ chạy khi đang ghi, archive của stream đã dừng tồn tại vô thời hạn | Cao | [services-14](services-14-dvr-retention-only-while-recording.md) |  |
|  | Hot-reload GeoIP giải phóng mmap khi lookup khác đang đọc, có thể làm chết process | Trung bình | [services-20](services-20-geoip-hot-reload-unmap-race.md) |  |
|  | Hầu hết event gửi cho hook có id rỗng và occurred_at bằng năm 0001 | Cao | [services-21](services-21-events-empty-id-and-occurred-at.md) |  |
|  | /metrics công khai làm lộ URL push kèm stream key | Cao | [services-25](services-25-metrics-dest-url-leaks-stream-key.md) |  |
|  | User read-only đọc được secret và tự cấp được token phát | Cao | [services-26](services-26-read-role-exposes-secrets.md) |  |
|  | vSnap/aSnap không reset khi SwitchInput, gây lệch A/V cố định sau failover | Cao | [transcoder-06](transcoder-06-stale-cross-track-snap.md) |  |
|  | Audio re-encode gán gần như cùng PTS cho mọi frame trong một PES nhiều ADTS | Cao | [transcoder-07](transcoder-07-audio-reencode-bundled-pes-pts.md) |  |
|  | Fallback decoder sang CPU trên đường GPU đưa frame phần mềm vào scale_cuda | Cao | [transcoder-09](transcoder-09-gpu-decoder-cpu-fallback.md) |  |
|  | Một access unit video lỗi làm chết cả subprocess | Trung bình | [transcoder-10](transcoder-10-single-corrupt-au-fatal.md) |  |
|  | Cổng keyframe chỉ nhận IDR nên nguồn H.264 open-GOP có thể không bao giờ ra hình | Trung bình | [transcoder-11](transcoder-11-h264-non-idr-gate.md) |  |
|  | framerate của profile và global.fps không đổi được tốc độ khung hình output | Cao | [transcoder-12](transcoder-12-framerate-not-applied.md) |  |
|  | resize_mode, sar và refs bị bỏ qua nên nguồn khác tỉ lệ luôn bị kéo méo | Cao | [transcoder-13](transcoder-13-resize-mode-ignored.md) |  |
|  | video.interlace không được áp dụng, nguồn interlaced không được deinterlace | Cao | [transcoder-14](transcoder-14-interlace-ignored.md) | [flow-config-28](flow-config-28-transcoder-fields-ignored.md) |
|  | Stream code dài vượt giới hạn đường dẫn Unix socket nên transcoder không bao giờ chạy | Cao | [transcoder-15](transcoder-15-socket-path-too-long.md) |  |
|  | Output AV1 và MPEG-2 bị gắn nhãn H.264 khi đưa vào buffer hub | Cao | [transcoder-16](transcoder-16-video-codec-mislabel.md) |  |

## Thấp

| | Lỗi | Tin cậy | File | Trùng với |
|---|---|---|---|---|
|  | Stream ABR copy và ABR mixer luôn báo Active dù upstream đã chết | Cao | [control-12](control-12-abr-status-never-degraded.md) |  |
|  | reloadTranscoderFull lỗi làm rò rỉ rendition buffer mới | Cao | [control-15](control-15-reload-failed-leaks-rendition-buffers.md) |  |
|  | Thống kê track của input không bao giờ reset, UI hiện bitrate và độ phân giải cũ | Cao | [control-16](control-16-input-track-stats-never-reset.md) |  |
|  | Callback exhausted và restored có thể đảo thứ tự, stream kẹt Degraded | Trung bình | [control-17](control-17-exhausted-restored-callback-reorder.md) |  |
|  | tryFailover không được tuần tự hoá, ingestor.Start có thể lệch với state.active | Trung bình | [control-18](control-18-tryfailover-not-serialized.md) |  |
|  | PAT/PMT do FromAV ép phát khi thêm track bị xóa trước khi gửi đi | Cao | [core-09](core-09-fromav-forced-psi-discarded.md) |  |
|  | FromAV không phát PCR cho stream chỉ có audio | Cao | [core-10](core-10-fromav-audio-only-no-pcr.md) |  |
|  | DTS=0 hợp lệ bị coi là thiếu, DTS trên wire lùi ở frame thứ hai khi có B-frame | Trung bình | [core-11](core-11-dts-zero-treated-as-missing.md) |  |
|  | CTO tính trên PTS/DTS chưa unwrap, B-frame mất CTO ở mốc wrap 26,5 giờ | Trung bình | [core-12](core-12-cto-computed-before-unwrap.md) |  |
|  | Subscribe chạy song song với Delete tạo subscriber mồ côi, không bao giờ nhận EOF | Trung bình | [core-13](core-13-subscribe-delete-orphan.md) |  |
|  | BestRenditionIndex coi profile giữ độ phân giải nguồn (0x0) là thấp nhất | Trung bình | [core-14](core-14-best-rendition-source-resolution.md) |  |
|  | Opacity=0 hiển thị watermark đục hoàn toàn thay vì trong suốt | Cao | [core-16](core-16-watermark-opacity-zero-opaque.md) |  |
|  | ParseAudioVolume chấp nhận NaN và Inf, audio đưa vào encoder bị hỏng | Trung bình | [core-18](core-18-audio-volume-nan-inf.md) |  |
|  | runtime.media sai với stream kế thừa transcoder từ template | Cao | [flow-config-18](flow-config-18-media-summary-raw-transcoder.md) |  |
|  | stream.started được phát trước stream.created, và create lỗi thì mất event | Cao | [flow-config-21](flow-config-21-stream-started-before-created.md) |  |
|  | GET rồi PUT /config/yaml nguyên văn có thể bị từ chối 422 | Cao | [flow-config-22](flow-config-22-yaml-round-trip-zero-default-rejected.md) |  |
|  | /config/defaults quảng bá port mặc định mà listener không áp dụng | Cao | [flow-config-23](flow-config-23-listener-port-default-mismatch.md) |  |
|  | Một stream reload lỗi khiến matcher prefix không được cập nhật | Cao | [flow-config-26](flow-config-26-template-put-reload-error-skips-refresh.md) |  |
| 🔁 | Series metric và lock theo stream tăng không giới hạn sau mỗi vòng đời | Cao | [flow-lifecycle-16](flow-lifecycle-16-per-stream-metrics-never-deleted.md) |  |
|  | Worker cũ trong pha handoff ghi đè trạng thái input đã bị chuyển đi | Trung bình | [flow-lifecycle-20](flow-lifecycle-20-stale-worker-recordpacket.md) |  |
|  | RTSP gán cùng PTS cho mọi AAC access unit trong một gói RTP | Cao | [ingest-21](ingest-21-rtsp-aac-same-pts.md) |  |
|  | Composition time âm của RTMP bị đọc thành số dương cực lớn | Trung bình | [ingest-22](ingest-22-rtmp-negative-cts.md) |  |
|  | Khoá push từ URL legacy không khớp đường định tuyến RTMP và không có SRT ingest | Cao | [ingest-23](ingest-23-legacy-push-key.md) |  |
|  | HTTP-TS không có timeout cho dial và TLS handshake | Cao | [ingest-24](ingest-24-httpts-no-dial-tls-timeout.md) |  |
|  | Bộ quét stats giả định chunk thẳng hàng 188 byte, panel tracks của HTTP-TS sai hoặc trống | Cao | [ingest-25](ingest-25-stats-unaligned-httpts.md) |  |
|  | Huỷ RTMP pull trong lúc đang dial làm rò rỉ phiên kết nối | Trung bình | [ingest-26](ingest-26-rtmp-dispose-during-dial-leak.md) |  |
|  | RTMP push gửi onMetaData không có `@setDataFrame` | Thấp | [publisher-09](publisher-09-push-metadata-no-setdataframe.md) |  |
| 🔁 | Nguyên nhân gốc của log `dash: video OOO push`: so sánh PTS trên queue thứ tự giải mã | Cao | [publisher-19](publisher-19-dash-ooo-log-root-cause.md) |  |
|  | Segment đầu của mọi shard ABR có tfdt=0 dù AST dùng chung | Trung bình | [publisher-20](publisher-20-dash-abr-first-tfdt-zero.md) |  |
|  | Playlist HLS live thiếu `EXT-X-DISCONTINUITY-SEQUENCE` | Cao | [publisher-25](publisher-25-hls-missing-discontinuity-sequence.md) |  |
|  | Ghi segment HLS lỗi làm lệch MEDIA-SEQUENCE và mất DISCONTINUITY | Cao | [publisher-26](publisher-26-hls-failed-write-media-sequence.md) |  |
|  | Stream chỉ có audio không bao giờ phát được qua RTSP và RTMP play | Cao | [publisher-33](publisher-33-audio-only-rtsp-rtmp-never-served.md) |  |
|  | Tắt HLS/DASH khi stream đang chạy để lại playlist cũ vẫn được phục vụ | Cao | [publisher-36](publisher-36-protocol-off-leaves-files.md) |  |
|  | recording_status luôn báo không có gap dù có failover hoặc mất nguồn | Cao | [services-15](services-15-dvr-gaps-never-populated.md) |  |
|  | Viewer kết nối dài biến mất vĩnh viễn khỏi tracker nếu nguồn ngừng quá idle timeout | Cao | [services-17](services-17-sessions-conn-reaped-during-source-stall.md) |  |
|  | Tắt sessions lúc đang chạy làm các session HLS/DASH và gauge đứng yên mãi | Cao | [services-18](services-18-sessions-disabled-leaves-stale-records.md) |  |
|  | Một viewer timeshift bị tính thành hai session, phần lớn byte gắn sai giao thức và sai cờ DVR | Cao | [services-19](services-19-sessions-dvr-fragments-counted-as-dash.md) |  |
|  | POST /hooks/{id}/test báo delivered cả khi đích HTTP không tồn tại | Cao | [services-22](services-22-hook-test-reports-delivered.md) |  |
|  | POST /hooks chấp nhận ID rỗng và ghi đè hook có sẵn | Cao | [services-23](services-23-hook-create-empty-or-duplicate-id.md) |  |
|  | Event cuối cùng lúc shutdown không bao giờ tới hook | Cao | [services-24](services-24-events-lost-on-shutdown.md) |  |
|  | Stream code trùng tiền tố admin tạo được nhưng output HTTP không thể truy cập | Cao | [services-27](services-27-reserved-stream-code-routing.md) |  |
|  | Stream có code kết thúc bằng track_N bị kiểm tra policy theo code cha | Cao | [services-28](services-28-mediaauth-track-suffix-policy-bypass.md) |  |
|  | Tham số timeshift không được validate, chèn được dòng tuỳ ý vào master playlist | Cao | [services-29](services-29-timeshift-master-playlist-injection.md) |  |
|  | Hai upload VOD cùng tên dùng chung file .tmp, có thể tạo file hỏng hoặc ghi đè | Cao | [services-31](services-31-vod-concurrent-upload-tmp-collision.md) |  |
|  | PlayURL và IngestURL của VOD hỏng với tên file có ký tự đặc biệt | Cao | [services-32](services-32-vod-urls-not-escaped.md) |  |
|  | Lệnh Stop không bao giờ tới subprocess nên encoder tail luôn bị mất | Cao | [transcoder-17](transcoder-17-stop-never-delivered.md) |  |
|  | SessionStart có thể chỉ tới một phần rendition sau khi chuyển input | Cao | [transcoder-18](transcoder-18-session-start-partial-renditions.md) |  |
|  | Packet SessionStart bị hub drop khiến transcoder bỏ lỡ SwitchInput | Trung bình | [transcoder-19](transcoder-19-dropped-session-start-misses-switch.md) |  |
|  | audio.normalize được truyền tới subprocess nhưng không được áp dụng | Cao | [transcoder-20](transcoder-20-audio-normalize-ignored.md) |  |
|  | Packet đưa vào decoder thiếu AV_INPUT_BUFFER_PADDING_SIZE | Trung bình | [transcoder-21](transcoder-21-packet-missing-padding.md) |  |
|  | hwdetect báo QSV và VAAPI sai trên máy có CPU Intel và GPU NVIDIA | Cao | [transcoder-22](transcoder-22-hwdetect-false-qsv-vaapi.md) |  |
