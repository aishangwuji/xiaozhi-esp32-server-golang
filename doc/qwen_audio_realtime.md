# Qwen Audio Realtime

管理员在“AI 配置 → Qwen Realtime 配置”中填写百炼 API Key、模型、音色和轮次检测方式。内置默认配置使用 `qwen-audio-3.1-realtime-plus` 与 `smart_turn`，不预填 API Key。配置完成后，在智能体表单开启“Qwen Audio Realtime”；配置选择留空时使用默认配置。

启用后，每个设备会话建立一条至百炼的 WebSocket 连接。设备上行的 Opus 或 PCM 音频转换为 16 kHz、16 位、单声道 PCM；百炼返回的 24 kHz PCM 音频按设备协商的格式下发。云端负责语音检测、识别、生成和合成。设备在 `realtime` 监听模式下持续上行音频时，上行与回复播放可以同时进行；播放期间的有效用户发言可打断回复。`smart_turn` 融合声学与语义判断有效轮次，可能把已检测到的声音判为无效轮次，因此本地播放等到百炼确认取消响应后才中断。`server_vad` 按语音起止自动触发回复。选择“手动提交”时，`listen:stop` 提交音频并请求回复；`abort` 用于客户端主动打断。

云端转写文本会作为 ASR 文本发给设备，并写入会话历史。回复文本与音频通过现有 TTS 播放队列下发。`server_vad` 检测到用户说话开始时会停止旧回复；`smart_turn` 等待百炼确认取消响应后再清除旧回复的待播音频，避免无效轮次误打断。

切换智能体配置后，让设备重新发送 hello 或重新连接，以创建使用新配置的会话。测试真实连接需要与所选模型区域匹配的百炼 API Key。

## 端到端验证

在 Linux 上启动主服务和 Manager，并给专用测试设备绑定启用了 Qwen Realtime 的智能体。准备 16 kHz、16 位、单声道 WAV，然后运行：

```bash
go run ./test/qwen_realtime_e2e -server ws://127.0.0.1:18991/xiaozhi/v1/ -device <测试设备 ID> -wav <语音样本.wav>
```

手动提交模式给配置设置 `turn_detection: "null"`，并在命令中加入 `-manual`。测试会检查非空转写、回复句子、TTS 结束事件，以及所有下行 Opus 帧均可解码且至少有一帧非静音音频。测试程序不打印对话文本。

要验证双工输入与播放重叠，使用 `smart_turn` 配置并加入 `-duplex`。测试客户端会在第一段回复音频开始后立即上行第二段语音，检查第二次转写和回复、两次播放结束，以及第二段输入确实早于第一段播放结束。
