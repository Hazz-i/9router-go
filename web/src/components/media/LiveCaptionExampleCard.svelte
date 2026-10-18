<script lang="ts">
  import { onMount } from 'svelte'
  import { api, buildLiveCaptionSocketUrl, getStoredAPIKey, type APIKey, type LiveCaptionCapability, type LiveCaptionFrame } from '../../api/client'
  import { copyToClipboard } from '../../lib/clipboard'
  import { getModelKind, getModelsByProviderId } from '../../lib/models'
  import { chunkWords, chunksToAss, chunksToSrt, type CaptionWord } from '../../lib/subtitleFormatter'
  import Card from '../../lib/ui/Card.svelte'

  interface Props {
    providerId: string
    providerAlias: string
    capability: LiveCaptionCapability
    apiKeys?: APIKey[]
  }

  let { providerId, providerAlias, capability, apiKeys = [] }: Props = $props()

  let captionModels = $derived(getModelsByProviderId(providerId).filter((model) => {
    const kinds = Array.isArray(model.kinds) ? model.kinds : []
    return kinds.includes('liveCaption') || kinds.includes('stt') || getModelKind(model) === 'stt'
  }))
  let selectedModel = $state('')
  let language = $state('id')
  let activeApiKey = $state('')
  let useTunnel = $state(false)
  let tunnelEndpoint = $state('')
  let isRecording = $state(false)
  let connectionStatus = $state<'idle' | 'connecting' | 'connected' | 'error'>('idle')
  let statusMessage = $state('')
  let transcript = $state<{ words: CaptionWord[]; partial: string }>({ words: [], partial: '' })
  let copiedWs = $state(false)
  let copiedSrt = $state(false)
  let copiedAss = $state(false)
  let localEndpoint = $state('http://localhost:20130')
  let transcriptContainer = $state<HTMLDivElement | null>(null)

  let socket: WebSocket | null = null
  let audioContext: AudioContext | null = null
  let mediaStream: MediaStream | null = null
  let processor: ScriptProcessorNode | null = null
  let audioSource: MediaStreamAudioSourceNode | null = null
  let mounted = true

  let effectiveEndpoint = $derived(useTunnel && tunnelEndpoint ? tunnelEndpoint : localEndpoint)
  let modelFull = $derived(`${providerAlias}/${selectedModel}`)
  let wsUrl = $derived(buildLiveCaptionSocketUrl(effectiveEndpoint, {
    model: modelFull,
    language,
    encoding: 'linear16',
    sample_rate: 16000,
    apiKey: activeApiKey,
  }))
  let subtitleChunks = $derived(chunkWords(transcript.words))
  let srt = $derived(chunksToSrt(subtitleChunks))
  let ass = $derived(chunksToAss(subtitleChunks))

  $effect(() => {
    if (transcript.words.length && transcriptContainer) {
      transcriptContainer.scrollTo({ top: transcriptContainer.scrollHeight, behavior: 'smooth' })
    }
  })

  $effect(() => {
    if (!captionModels.some((model) => model.id === selectedModel)) selectedModel = captionModels[0]?.id || ''
    if (!activeApiKey) {
      const active = apiKeys.find((key) => key.isActive !== 0 && key.key)
      if (active?.key) activeApiKey = active.key
    }
  })

  function stopAudio() {
    if (processor) {
      processor.disconnect()
      processor.onaudioprocess = null
      processor = null
    }
    audioSource?.disconnect()
    audioSource = null
    if (audioContext) {
      void audioContext.close().catch(() => {})
      audioContext = null
    }
    mediaStream?.getTracks().forEach((track) => track.stop())
    mediaStream = null
  }

  function stopRecording() {
    isRecording = false
    stopAudio()
    const current = socket
    socket = null
    if (current) {
      try {
        if (current.readyState === WebSocket.OPEN) current.send(JSON.stringify({ type: 'finalize' }))
      } catch {
        // A broken socket should not prevent local microphone cleanup.
      }
    }
    if (current && current.readyState < WebSocket.CLOSING) {
      current.close(1000, 'User stopped recording')
    }
    connectionStatus = 'idle'
  }

  function floatToPcm16(input: Float32Array): ArrayBuffer {
    const buffer = new ArrayBuffer(input.length * 2)
    const view = new DataView(buffer)
    for (let index = 0; index < input.length; index++) {
      const sample = Math.max(-1, Math.min(1, input[index]))
      view.setInt16(index * 2, sample < 0 ? sample * 0x8000 : sample * 0x7fff, true)
    }
    return buffer
  }

  function handleFrame(frame: LiveCaptionFrame) {
    switch (frame.type) {
      case 'ready':
        statusMessage = `Ready. Upstream provider: ${frame.upstream}`
        break
      case 'partial':
        transcript.partial = frame.text || ''
        break
      case 'word':
        transcript.words = [...transcript.words, frame]
        transcript.partial = ''
        break
      case 'chunk':
        transcript.partial = ''
        break
      case 'error':
        statusMessage = `Error: ${frame.error}`
        connectionStatus = 'error'
        break
    }
  }

  async function startRecording() {
    try {
      connectionStatus = 'connecting'
      statusMessage = 'Requesting microphone permission...'
      mediaStream = await navigator.mediaDevices.getUserMedia({
        audio: { channelCount: 1, sampleRate: 16000, echoCancellation: true, noiseSuppression: true },
      })
      if (!mounted) {
        mediaStream.getTracks().forEach((track) => track.stop())
        mediaStream = null
        return
      }
      statusMessage = 'Connecting to NineRouter WebSocket...'
      const ws = new WebSocket(wsUrl)
      socket = ws
      ws.binaryType = 'arraybuffer'

      ws.onopen = () => {
        try {
          audioContext = new AudioContext({ sampleRate: 16000 })
          audioSource = audioContext.createMediaStreamSource(mediaStream as MediaStream)
          processor = audioContext.createScriptProcessor(4096, 1, 1)
          const currentProcessor = processor
          currentProcessor.onaudioprocess = (event) => {
            if (ws.readyState === WebSocket.OPEN) ws.send(floatToPcm16(event.inputBuffer.getChannelData(0)))
          }
          audioSource.connect(currentProcessor)
          currentProcessor.connect(audioContext.destination)
          connectionStatus = 'connected'
          statusMessage = 'WebSocket connected. Starting audio stream...'
          isRecording = true
        } catch (error) {
          stopAudio()
          connectionStatus = 'error'
          statusMessage = `Audio setup failed: ${error instanceof Error ? error.message : String(error)}`
          ws.close()
        }
      }
      ws.onmessage = (event: MessageEvent<string>) => {
        try {
          handleFrame(JSON.parse(event.data) as LiveCaptionFrame)
        } catch {
          // Ignore non-JSON socket messages.
        }
      }
      ws.onerror = () => {
        connectionStatus = 'error'
        statusMessage = 'WebSocket connection failed.'
      }
      ws.onclose = (event) => {
        stopAudio()
        socket = null
        isRecording = false
        connectionStatus = 'idle'
        statusMessage = event.reason ? `Connection closed: ${event.reason}` : 'Connection closed.'
      }
    } catch (error) {
      stopAudio()
      connectionStatus = 'error'
      statusMessage = `Microphone error: ${error instanceof Error ? error.message : String(error)}`
    }
  }

  function clearTranscript() {
    transcript = { words: [], partial: '' }
  }

  async function copy(value: string, format: 'ws' | 'srt' | 'ass') {
    await copyToClipboard(value)
    if (format === 'ws') copiedWs = true
    else if (format === 'srt') copiedSrt = true
    else copiedAss = true
    setTimeout(() => {
      if (format === 'ws') copiedWs = false
      else if (format === 'srt') copiedSrt = false
      else copiedAss = false
    }, 1800)
  }

  onMount(() => {
    mounted = true
    localEndpoint = window.location.origin
    activeApiKey = getStoredAPIKey() || apiKeys.find((key) => key.isActive !== 0 && key.key)?.key || ''
    api.getTunnelStatus().then((status) => {
      tunnelEndpoint = status.tunnel?.publicUrl || status.tunnel?.tunnelUrl || status.tailscale?.tunnelUrl || ''
    })
    return () => {
      mounted = false
      stopRecording()
    }
  })
</script>

<Card class="shadow-[var(--shadow-warm)]">
  <div class="flex flex-col gap-5">
    <div class="flex flex-col gap-3 border-b border-border/40 pb-4 sm:flex-row sm:items-center sm:justify-between">
      <div>
        <div class="flex flex-wrap items-center gap-2">
          <span class="material-symbols-outlined text-[22px] text-primary">closed_caption</span>
          <h2 class="text-base font-semibold text-text-main">Live Caption & Streaming Playground</h2>
          <span class="rounded-full bg-primary/10 px-2 py-0.5 text-[10px] font-semibold tracking-wide text-primary">REALTIME WS</span>
        </div>
        <p class="mt-1 text-xs text-text-muted">Stream microphone audio through 9router and follow the transcript as it arrives.</p>
      </div>
      <div class="flex flex-wrap items-center gap-2">
        {#if !isRecording}
          <button type="button" onclick={startRecording} disabled={connectionStatus === 'connecting' || !selectedModel} class="inline-flex items-center gap-1.5 rounded-[10px] bg-primary px-3 py-2 text-xs font-semibold text-white transition-colors hover:bg-primary-hover disabled:cursor-not-allowed disabled:opacity-50">
            <span class="material-symbols-outlined text-[16px]">mic</span>{connectionStatus === 'connecting' ? 'Connecting…' : 'Start Microphone'}
          </button>
        {:else}
          <button type="button" onclick={stopRecording} class="inline-flex items-center gap-1.5 rounded-[10px] border border-red-500/25 bg-red-500/10 px-3 py-2 text-xs font-semibold text-red-600 transition-colors hover:bg-red-500/15 dark:text-red-400">
            <span class="material-symbols-outlined text-[16px]">stop</span>Stop Streaming
          </button>
        {/if}
        {#if transcript.words.length}
          <button type="button" onclick={clearTranscript} class="inline-flex items-center gap-1 rounded-[10px] border border-border px-3 py-2 text-xs text-text-muted transition-colors hover:bg-surface-2 hover:text-text-main">
            <span class="material-symbols-outlined text-[16px]">delete_sweep</span>Clear
          </button>
        {/if}
      </div>
    </div>

    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
      <label class="flex min-w-0 flex-col gap-1.5 text-xs font-medium text-text-muted">
        Model
        <select bind:value={selectedModel} disabled={isRecording || connectionStatus === 'connecting'} class="w-full rounded-[10px] border border-border bg-surface px-3 py-2 font-mono text-xs text-text-main focus:border-primary focus:outline-none disabled:opacity-60">
          {#each captionModels as model (model.id)}
            <option value={model.id}>{model.name || model.id}</option>
          {/each}
        </select>
      </label>
      <label class="flex min-w-0 flex-col gap-1.5 text-xs font-medium text-text-muted">
        Language
        <input bind:value={language} disabled={isRecording || connectionStatus === 'connecting'} placeholder="id, en, etc." class="w-full rounded-[10px] border border-border bg-surface px-3 py-2 font-mono text-xs text-text-main focus:border-primary focus:outline-none disabled:opacity-60" />
      </label>
      {#if tunnelEndpoint}
        <div class="flex flex-col gap-1.5 text-xs font-medium text-text-muted">
          Endpoint source
          <div class="flex gap-2">
            <button type="button" disabled={isRecording} onclick={() => (useTunnel = false)} class="rounded-[10px] border px-3 py-2 text-xs transition-colors {useTunnel ? 'border-border text-text-muted hover:bg-surface-2' : 'border-primary/40 bg-primary/10 text-primary'}">Local</button>
            <button type="button" disabled={isRecording} onclick={() => (useTunnel = true)} class="rounded-[10px] border px-3 py-2 text-xs transition-colors {useTunnel ? 'border-primary/40 bg-primary/10 text-primary' : 'border-border text-text-muted hover:bg-surface-2'}">Tunnel</button>
          </div>
        </div>
      {/if}
    </div>

    <div class="flex min-w-0 items-center gap-2 text-xs" aria-live="polite">
      <span class="size-2.5 shrink-0 rounded-full {isRecording ? 'animate-pulse bg-red-500' : connectionStatus === 'connected' ? 'bg-emerald-500' : connectionStatus === 'error' ? 'bg-amber-500' : 'bg-border'}"></span>
      <span class="shrink-0 font-mono text-text-muted">{isRecording ? 'STREAMING AUDIO' : connectionStatus.toUpperCase()}</span>
      {#if statusMessage}<span class="truncate text-[11px] text-text-muted/80">· {statusMessage}</span>{/if}
      {#if capability.format}<span class="ml-auto hidden shrink-0 text-[10px] uppercase tracking-wide text-text-subtle sm:inline">{capability.format}</span>{/if}
    </div>

    <div bind:this={transcriptContainer} class="flex min-h-[150px] max-h-[260px] flex-col justify-between overflow-y-auto rounded-[10px] border border-border bg-surface-2/50 p-4 shadow-inner" aria-live="polite">
      <p class="text-base leading-relaxed text-text-main sm:text-lg">
        {#if transcript.words.length === 0 && !transcript.partial}
          <span class="text-xs italic text-text-muted">{isRecording ? 'Listening… Speak into your microphone' : "Choose a model and start the microphone to begin live captioning."}</span>
        {:else}
          {#if transcript.words.length}<span class="font-medium">{transcript.words.map((word) => word.word).join(' ')}</span>{/if}
          {#if transcript.partial}<span class="italic text-text-muted">{transcript.words.length ? ' ' : ''}{transcript.partial}</span>{/if}
        {/if}
      </p>
      {#if transcript.words.length}
        <div class="mt-4 flex flex-col gap-3 border-t border-border/60 pt-3 text-[11px] text-text-muted sm:flex-row sm:items-center sm:justify-between">
          <span>{transcript.words.length} words detected</span>
          <div class="flex flex-wrap items-center gap-2">
            <button type="button" onclick={() => copy(srt, 'srt')} class="rounded-[10px] border border-border bg-surface px-2.5 py-1.5 transition-colors hover:bg-surface-2">{copiedSrt ? '✓ Copied SRT' : 'Copy .SRT'}</button>
            <button type="button" onclick={() => copy(ass, 'ass')} class="rounded-[10px] border border-border bg-surface px-2.5 py-1.5 transition-colors hover:bg-surface-2">{copiedAss ? '✓ Copied ASS' : 'Copy .ASS (Karaoke)'}</button>
          </div>
        </div>
      {/if}
    </div>

    <div class="flex flex-col gap-2 border-t border-border/60 pt-3">
      <div class="flex items-center justify-between gap-3 text-xs">
        <span class="font-medium text-text-muted">WebSocket Endpoint URL</span>
        <button type="button" onclick={() => copy(wsUrl, 'ws')} class="inline-flex shrink-0 items-center gap-1 text-primary hover:underline">
          <span class="material-symbols-outlined text-[14px]">{copiedWs ? 'check' : 'content_copy'}</span>{copiedWs ? 'Copied' : 'Copy WS URL'}
        </button>
      </div>
      <code class="break-all rounded-[10px] border border-border bg-sidebar p-3 font-mono text-[11px] text-text-muted select-all">{wsUrl}</code>
    </div>
  </div>
</Card>
