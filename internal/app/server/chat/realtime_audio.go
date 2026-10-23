package chat

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"

	data_client "xiaozhi-esp32-server-golang/internal/data/client"
	domainaudio "xiaozhi-esp32-server-golang/internal/domain/audio"
	"xiaozhi-esp32-server-golang/internal/domain/eventbus"
	"xiaozhi-esp32-server-golang/internal/domain/realtime"
	log "xiaozhi-esp32-server-golang/logger"
)

// realtimeAudioSession bridges Xiaozhi device audio to Qwen's native audio
// conversation. The normal ASR/LLM/TTS generation pipeline is not started.
type realtimeAudioSession struct {
	owner    *ChatSession
	provider realtime.ProviderClient
	cfg      realtime.Config
	ctx      context.Context
	cancel   context.CancelFunc

	inputDecoder   *domainaudio.AudioProcesser
	outputEncoder  *domainaudio.AudioProcesser
	outputMu       sync.Mutex
	outputPCM      []int16
	audioStarted   atomic.Bool
	responseActive atomic.Bool
	skipUntilDone  atomic.Bool
	closeOnce      sync.Once

	assistantText strings.Builder // only accessed by consumeEvents
	pendingText   string
}

func (s *ChatSession) isRealtimeAudio() bool {
	return s != nil && s.clientState != nil && s.clientState.DeviceConfig.RealtimeEnabled
}

func newRealtimeAudioSession(owner *ChatSession) (*realtimeAudioSession, error) {
	if owner == nil || owner.clientState == nil {
		return nil, errors.New("realtime 会话状态为空")
	}
	raw := owner.clientState.DeviceConfig.Realtime
	if raw.Provider != realtime.ProviderAliyunQwenRealtime {
		return nil, fmt.Errorf("暂不支持 realtime provider: %s", raw.Provider)
	}
	cfg, err := realtime.ParseAliyunQwenConfig(raw.Config)
	if err != nil {
		return nil, err
	}
	if cfg.Instructions == "" {
		cfg.Instructions = owner.clientState.SystemPrompt
	}
	if memory := strings.TrimSpace(owner.clientState.MemoryContext); memory != "" {
		cfg.Instructions = strings.TrimSpace(cfg.Instructions) + "\n用户个性化信息:\n" + memory
	}
	for _, message := range owner.clientState.GetMessages(20) {
		if message == nil || strings.TrimSpace(message.Content) == "" {
			continue
		}
		role := string(message.Role)
		if role == "user" || role == "assistant" || role == "system" {
			cfg.InitialMessages = append(cfg.InitialMessages, realtime.ConversationMessage{Role: role, Text: message.Content})
		}
	}
	client, err := realtime.NewProviderClient(raw.Provider, cfg)
	if err != nil {
		return nil, err
	}
	return &realtimeAudioSession{owner: owner, provider: client, cfg: cfg}, nil
}

func (r *realtimeAudioSession) Start(parent context.Context) error {
	r.ctx, r.cancel = context.WithCancel(parent)
	state := r.owner.clientState
	input := state.InputAudioFormat
	if input.Format == "" {
		input.Format = "opus"
		state.InputAudioFormat = input
	}
	if strings.EqualFold(input.Format, "opus") {
		decoder, err := domainaudio.GetAudioProcesser(input.SampleRate, input.Channels, input.FrameDuration)
		if err != nil {
			return fmt.Errorf("创建 realtime 输入解码器失败: %w", err)
		}
		r.inputDecoder = decoder
	} else if !strings.EqualFold(input.Format, "pcm") {
		return fmt.Errorf("不支持的 realtime 输入格式: %s", input.Format)
	}
	output := state.OutputAudioFormat
	if strings.EqualFold(output.Format, "opus") {
		encoder, err := domainaudio.GetAudioProcesser(output.SampleRate, output.Channels, output.FrameDuration)
		if err != nil {
			return fmt.Errorf("创建 realtime 输出编码器失败: %w", err)
		}
		r.outputEncoder = encoder
	} else if !strings.EqualFold(output.Format, "pcm") {
		return fmt.Errorf("不支持的 realtime 输出格式: %s", output.Format)
	}
	if err := r.provider.Start(r.ctx); err != nil {
		r.cancel()
		return err
	}
	go r.consumeEvents()
	return nil
}

func (r *realtimeAudioSession) FeedAudio(encoded []byte) bool {
	if r == nil || r.ctx.Err() != nil || len(encoded) == 0 {
		return false
	}
	format := r.owner.clientState.InputAudioFormat
	var pcm []int16
	if strings.EqualFold(format.Format, "pcm") {
		pcm = realtimePCM16Samples(encoded)
	} else {
		decoded := make([]int16, format.SampleRate*format.Channels*120/1000)
		n, err := r.inputDecoder.Decoder(encoded, decoded)
		if err != nil {
			log.Warnf("realtime 输入音频解码失败: device=%s err=%v", r.owner.clientState.DeviceID, err)
			return false
		}
		pcm = decoded[:n*format.Channels]
	}
	pcm = realtimeConvertChannels(pcm, format.Channels, 1)
	pcm = realtimeResamplePCM16(pcm, format.SampleRate, 16000)
	if err := r.provider.SendAudio(realtimePCM16Bytes(pcm)); err != nil {
		log.Warnf("realtime 输入音频发送失败: device=%s err=%v", r.owner.clientState.DeviceID, err)
		return false
	}
	return true
}

func (r *realtimeAudioSession) consumeEvents() {
	for {
		select {
		case <-r.ctx.Done():
			return
		case event, ok := <-r.provider.Events():
			if !ok {
				if r.ctx.Err() == nil {
					log.Warnf("Qwen Realtime 事件流意外关闭: device=%s", r.owner.clientState.DeviceID)
					r.owner.CloseWithReason(chatSessionCloseReasonFatalError)
				}
				return
			}
			if event.Err != nil {
				log.Warnf("Qwen Realtime 事件错误: device=%s type=%s err=%v", r.owner.clientState.DeviceID, event.Type, event.Err)
				if event.Fatal {
					r.owner.CloseWithReason(chatSessionCloseReasonFatalError)
					return
				}
			}
			r.handleEvent(event)
		}
	}
}

func (r *realtimeAudioSession) handleEvent(event realtime.Event) {
	switch event.Type {
	case "response.created":
		r.responseActive.Store(true)
		r.assistantText.Reset()
		r.pendingText = ""
	case "response.audio.delta":
		if !r.skipUntilDone.Load() && len(event.Audio) != 0 {
			r.emitAudio(event.Audio)
		}
	case "response.audio_transcript.delta":
		if !r.skipUntilDone.Load() {
			r.assistantText.WriteString(event.Text)
			r.pendingText += event.Text
			r.emitCompleteSentences()
		}
	case "response.audio_transcript.done":
		if !r.skipUntilDone.Load() && r.assistantText.Len() == 0 {
			r.assistantText.WriteString(event.Text)
			r.pendingText += event.Text
			r.emitCompleteSentences()
		}
	case "conversation.item.input_audio_transcription.completed":
		text := strings.TrimSpace(event.Text)
		if text != "" {
			_ = r.owner.serverTransport.SendAsrResult(text)
			r.saveMessage(schema.UserMessage(text))
		}
	case "input_audio_buffer.speech_started":
		// In smart_turn this event is provisional: Qwen may later mark the
		// speech as turn_invalid. Keep playing until Qwen cancels the response.
		if r.cfg.TurnDetection == "server_vad" {
			r.interruptLocalPlayback("realtime speech_started")
		}
	case "response.done":
		if event.Status != "cancelled" && event.Status != "failed" && !r.skipUntilDone.Load() {
			r.flushOutput()
			r.emitSentence(strings.TrimSpace(r.pendingText))
			r.pendingText = ""
			text := strings.TrimSpace(r.assistantText.String())
			if text != "" {
				r.saveMessage(schema.AssistantMessage(text, nil))
			}
			r.stopPlayback()
		} else if event.Status == "cancelled" || event.Status == "failed" {
			// The provider has confirmed interruption; discard frames already
			// queued for local playback as well as the partial PCM frame.
			r.interruptLocalPlayback("realtime response " + event.Status)
		} else {
			r.clearOutputBuffer()
			r.stopPlayback()
		}
		r.pendingText = ""
		r.responseActive.Store(false)
		r.skipUntilDone.Store(false)
	}
}

func (r *realtimeAudioSession) emitCompleteSentences() {
	for {
		index := strings.IndexAny(r.pendingText, "。！？!?；;\n")
		if index < 0 {
			return
		}
		_, width := utf8.DecodeRuneInString(r.pendingText[index:])
		r.emitSentence(strings.TrimSpace(r.pendingText[:index+width]))
		r.pendingText = r.pendingText[index+width:]
	}
}

func (r *realtimeAudioSession) emitSentence(sentence string) {
	if sentence == "" {
		return
	}
	_ = r.owner.ttsManager.EnqueueMediaSentenceStart(r.ctx, sentence, nil)
	_ = r.owner.ttsManager.EnqueueMediaSentenceEnd(r.ctx, sentence, nil, nil)
}

func (r *realtimeAudioSession) saveMessage(message *schema.Message) {
	if message == nil {
		return
	}
	r.owner.clientState.AddMessage(message)
	eventbus.Get().Publish(eventbus.TopicAddMessage, &eventbus.AddMessageEvent{
		ClientState: r.owner.clientState, Msg: *message,
		MessageID: fmt.Sprintf("realtime-%s-%d", r.owner.clientState.SessionID, time.Now().UnixNano()),
		Timestamp: time.Now(),
	})
}

func (r *realtimeAudioSession) startPlayback() {
	if r.audioStarted.CompareAndSwap(false, true) {
		r.owner.ttsManager.EnqueueTtsStartWithReason(r.ctx, "realtime response start")
	}
}

func (r *realtimeAudioSession) stopPlayback() {
	if r.audioStarted.CompareAndSwap(true, false) {
		r.owner.ttsManager.EnqueueTtsStopWithReason(r.ctx, "realtime response done")
	}
}

func (r *realtimeAudioSession) emitAudio(raw []byte) {
	state := r.owner.clientState
	pcm := realtimePCM16Samples(raw)
	pcm = realtimeResamplePCM16(pcm, 24000, state.OutputAudioFormat.SampleRate)
	pcm = realtimeConvertChannels(pcm, 1, state.OutputAudioFormat.Channels)
	if strings.EqualFold(state.OutputAudioFormat.Format, "pcm") {
		r.startPlayback()
		_ = r.owner.ttsManager.EnqueueMediaFrame(r.ctx, realtimePCM16Bytes(pcm), nil)
		return
	}
	r.outputMu.Lock()
	defer r.outputMu.Unlock()
	r.outputPCM = append(r.outputPCM, pcm...)
	frameSamples := state.OutputAudioFormat.SampleRate * state.OutputAudioFormat.Channels * state.OutputAudioFormat.FrameDuration / 1000
	for frameSamples > 0 && len(r.outputPCM) >= frameSamples {
		frame := r.outputPCM[:frameSamples]
		r.encodeFrame(frame)
		r.outputPCM = r.outputPCM[frameSamples:]
	}
}

func (r *realtimeAudioSession) encodeFrame(frame []int16) {
	encoded := make([]byte, 4096)
	n, err := r.outputEncoder.Encoder(frame, encoded)
	if err != nil {
		log.Warnf("realtime 输出音频编码失败: %v", err)
		return
	}
	if n > 0 {
		r.startPlayback()
		_ = r.owner.ttsManager.EnqueueMediaFrame(r.ctx, encoded[:n], nil)
	}
}

func (r *realtimeAudioSession) flushOutput() {
	if r.outputEncoder == nil {
		return
	}
	r.outputMu.Lock()
	defer r.outputMu.Unlock()
	if len(r.outputPCM) == 0 {
		return
	}
	format := r.owner.clientState.OutputAudioFormat
	frameSamples := format.SampleRate * format.Channels * format.FrameDuration / 1000
	if frameSamples > 0 {
		frame := make([]int16, frameSamples)
		copy(frame, r.outputPCM)
		r.encodeFrame(frame)
	}
	r.outputPCM = nil
}

func (r *realtimeAudioSession) clearOutputBuffer() {
	r.outputMu.Lock()
	r.outputPCM = nil
	r.outputMu.Unlock()
}

func (r *realtimeAudioSession) interruptLocalPlayback(reason string) {
	if r.responseActive.Load() || r.audioStarted.Load() {
		r.skipUntilDone.Store(true)
	}
	r.clearOutputBuffer()
	if r.audioStarted.Swap(false) {
		r.owner.StopAssistantOutputAfterAsrWithReason(true, reason)
	}
}

func (r *realtimeAudioSession) StartListen() error {
	state := r.owner.clientState
	state.SetClientVoiceStop(false)
	state.SetStatus(data_client.ClientStatusListening)
	state.SetListenPhase(data_client.ListenPhaseListening)
	state.StartAudioIdleWindow(time.Now())
	return r.provider.StartAudioActivity()
}

func (r *realtimeAudioSession) StopListen() error {
	state := r.owner.clientState
	state.SetClientVoiceStop(true)
	state.SetStatus(data_client.ClientStatusListenStop)
	state.SetListenPhase(data_client.ListenPhaseIdle)
	if r.cfg.TurnDetection == "null" {
		return r.provider.CommitAudio()
	}
	return nil
}

func (r *realtimeAudioSession) Abort() error {
	r.skipUntilDone.Store(true)
	r.interruptLocalPlayback("realtime abort")
	return r.provider.CancelResponse()
}

func (r *realtimeAudioSession) Close() {
	r.closeOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		_ = r.provider.Close()
	})
}

func realtimePCM16Samples(raw []byte) []int16 {
	out := make([]int16, len(raw)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	return out
}

func realtimePCM16Bytes(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(sample))
	}
	return out
}

func realtimeResamplePCM16(samples []int16, sourceRate, targetRate int) []int16 {
	if len(samples) == 0 || sourceRate <= 0 || targetRate <= 0 || sourceRate == targetRate {
		return samples
	}
	targetLength := len(samples) * targetRate / sourceRate
	if targetLength == 0 {
		return nil
	}
	out := make([]int16, targetLength)
	for i := range out {
		position := float64(i) * float64(len(samples)-1) / float64(max(targetLength-1, 1))
		left := int(position)
		right := min(left+1, len(samples)-1)
		out[i] = int16(float64(samples[left]) + (float64(samples[right])-float64(samples[left]))*(position-float64(left)))
	}
	return out
}

func realtimeConvertChannels(samples []int16, sourceChannels, targetChannels int) []int16 {
	if sourceChannels <= 0 || targetChannels <= 0 || sourceChannels == targetChannels {
		return samples
	}
	out := make([]int16, len(samples)/sourceChannels*targetChannels)
	for frame := 0; frame < len(out)/targetChannels; frame++ {
		for channel := 0; channel < targetChannels; channel++ {
			out[frame*targetChannels+channel] = samples[frame*sourceChannels+min(channel, sourceChannels-1)]
		}
	}
	return out
}
