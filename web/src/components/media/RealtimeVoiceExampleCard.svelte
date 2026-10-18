<script lang="ts">
  import { onMount } from 'svelte'
  import { api, getStoredAPIKey, type APIKey } from '../../api/client'
  import { copyToClipboard } from '../../lib/clipboard'
  import { getModelsByProviderId, type ProviderModel } from '../../lib/models'
  import Card from '../../lib/ui/Card.svelte'

  interface RealtimeOption {
    id: string
    name?: string
  }

  interface RealtimeProviderOption {
    type: string
    name?: string
    models?: RealtimeOption[]
    voices?: RealtimeOption[]
    customVoice?: string
  }

  interface RealtimeVoiceConfig {
    defaultModel?: string
    supportedVoices?: string[]
    think?: { provider?: { type: string; model: string }; providers?: RealtimeProviderOption[] }
    speak?: { provider?: { type: string; model: string }; providers?: RealtimeProviderOption[] }
    listen?: { provider?: { type: string; model: string }; providers?: RealtimeProviderOption[] }
  }

  interface Props {
    providerId: string
    providerAlias: string
    realtimeConfig?: RealtimeVoiceConfig
    apiKeys?: APIKey[]
  }

  interface TranscriptMessage {
    id: number
    role: 'assistant' | 'user'
    text: string
    isDone: boolean
  }

  type CallStatus = 'idle' | 'connecting' | 'connected' | 'speaking' | 'listening' | 'error'
  type SnippetLanguage = 'nodejs' | 'python' | 'cpp' | 'wscat'

  let { providerId, providerAlias, realtimeConfig = {}, apiKeys = [] }: Props = $props()

  const GEMINI_VOICES = [
    { id: 'Puck', label: 'Puck (Natural & Expressive)' },
    { id: 'Charon', label: 'Charon (Calm & Deep)' },
    { id: 'Kore', label: 'Kore (Warm & Friendly)' },
    { id: 'Fenrir', label: 'Fenrir (Energetic)' },
    { id: 'Aoede', label: 'Aoede (Clear & Soft)' },
  ]

  let realtimeModels = $derived(getModelsByProviderId(providerId).filter((model: ProviderModel) => {
    const kinds = model.kinds
    return (Array.isArray(kinds) && kinds.includes('realtime')) || model.kind === 'realtime'
  }))
  let realtimeModel = $derived(
    realtimeConfig.defaultModel || realtimeModels[0]?.id || ''
  )
  let isDeepgram = $derived(providerId === 'deepgram')
  let providerName = $derived(providerId === 'gemini' ? 'Gemini Live' : providerId === 'deepgram' ? 'Deepgram Voice Agent' : `${providerAlias} Realtime`)
  let voices = $derived(providerId === 'gemini' ? GEMINI_VOICES : (realtimeConfig.supportedVoices || []).map((id) => ({ id, label: id })))
  let thinkProviders = $derived(realtimeConfig.think?.providers || [])
  let thinkDefaultProvider = $derived(realtimeConfig.think?.provider?.type || '')
  let thinkDefaultModel = $derived(realtimeConfig.think?.provider?.model || '')

  function realtimeOptions(providers: RealtimeProviderOption[] | undefined, field: 'models' | 'voices', includeCustom = false) {
    return (providers || []).flatMap((provider) => [
      ...(provider[field] || []).map((option) => ({
        value: `${provider.type}/${option.id}`,
        label: `${provider.name || provider.type} · ${option.name || option.id}`,
      })),
      ...(includeCustom && (field === 'models' || provider.customVoice)
        ? [{ value: `${provider.type}/custom:`, label: `${provider.name || provider.type} · Custom…` }]
        : []),
    ])
  }

  const speakOptions = $derived(realtimeOptions(realtimeConfig.speak?.providers, 'voices', true))
  const listenOptions = $derived(realtimeOptions(realtimeConfig.listen?.providers, 'models', true))
  let selectedVoice = $state('Puck')
  let selectedThinkProvider = $state('')
  let selectedThinkModel = $state('')
  let selectedSpeak = $state('')
  let selectedListen = $state('')
  let customThinkModel = $state('')
  let customSpeakId = $state('')
  let customListenId = $state('')
  let greeting = $state('')
  let instructions = $state('You are a friendly, helpful AI voice assistant. Speak naturally and concisely in Indonesian or English.')
  let apiKey = $state('')
  let useTunnel = $state(false)
  let localEndpoint = $state('http://localhost:20130')
  let tunnelEndpoint = $state('')
  let calling = $state(false)
  let connectionStatus = $state<CallStatus>('idle')
  let statusMessage = $state('')
  let messages = $state<TranscriptMessage[]>([])
  let micVolume = $state(0)
  let selectedLanguage = $state<SnippetLanguage>('nodejs')
  let copiedWs = $state(false)
  let copiedCode = $state(false)
  let transcriptElement = $state<HTMLDivElement | null>(null)

  let socket: WebSocket | null = null
  let audioContextIn: AudioContext | null = null
  let audioContextOut: AudioContext | null = null
  let mediaStream: MediaStream | null = null
  let processor: ScriptProcessorNode | null = null
  let audioSource: MediaStreamAudioSourceNode | null = null
  let analyser: AnalyserNode | null = null
  let nextPlayTime = 0
  let activeSources: AudioBufferSourceNode[] = []
  let volumeFrame = 0
  let mounted = true
  let messageId = 0

  $effect(() => {
    if (!selectedThinkProvider && thinkDefaultProvider) selectedThinkProvider = thinkDefaultProvider
    if (!selectedThinkModel && thinkDefaultModel) selectedThinkModel = thinkDefaultModel
    if (!selectedSpeak && realtimeConfig.speak?.provider) selectedSpeak = `${realtimeConfig.speak.provider.type}/${realtimeConfig.speak.provider.model}`
    if (!selectedListen && realtimeConfig.listen?.provider) selectedListen = `${realtimeConfig.listen.provider.type}/${realtimeConfig.listen.provider.model}`
  })

  let endpoint = $derived(useTunnel && tunnelEndpoint ? tunnelEndpoint : localEndpoint)
  let wsUrl = $derived.by(() => {
    const url = new URL('/v1/realtime', endpoint || 'http://localhost:20130')
    url.protocol = url.protocol === 'https:' || url.protocol === 'wss:' ? 'wss:' : 'ws:'
    const model = realtimeModel.includes('/') ? realtimeModel : `${providerId}/${realtimeModel}`
    url.searchParams.set('model', model)
    if (voices.length) url.searchParams.set('voice', selectedVoice)
    if (isDeepgram) {
      const thinkModel = selectedThinkModel === '__custom__' ? customThinkModel.trim() : selectedThinkModel
      if (selectedThinkProvider && thinkModel) {
        url.searchParams.set('think', `${selectedThinkProvider}/${selectedThinkModel === '__custom__' ? `custom:${thinkModel}` : thinkModel}`)
      }
      if (selectedSpeak && (!selectedSpeak.endsWith('/custom:') || customSpeakId.trim())) {
        url.searchParams.set('speak', `${selectedSpeak}${selectedSpeak.endsWith('/custom:') ? customSpeakId.trim() : ''}`)
      }
      if (selectedListen && (!selectedListen.endsWith('/custom:') || customListenId.trim())) {
        url.searchParams.set('listen', `${selectedListen}${selectedListen.endsWith('/custom:') ? customListenId.trim() : ''}`)
      }
      if (greeting) url.searchParams.set('greeting', greeting)
    }
    if (apiKey) url.searchParams.set('apiKey', apiKey)
    return url.toString()
  })
  let badgeText = $derived(({ idle: 'Ready', connecting: 'Connecting', connected: 'Connected', speaking: 'AI Speaking', listening: 'Listening', error: 'Error' } satisfies Record<CallStatus, string>)[connectionStatus])
  let baseWsUrl = $derived(wsUrl.split('?')[0])
  let snippetParams = $derived.by(() => {
    const params = new URLSearchParams(wsUrl.split('?')[1] || '')
    params.delete('apiKey')
    return params.toString()
  })
  let deviceParams = $derived.by(() => {
    const params = new URLSearchParams(snippetParams)
    params.set('apiKey', apiKey || 'YOUR_9ROUTER_KEY')
    return params.toString()
  })
  let currentSnippet = $derived.by(() => {
    const url = `${baseWsUrl}?${snippetParams}`
    const deviceUrl = `${baseWsUrl}?${deviceParams}`
    const voiceSetting = voices.length ? `      voice: "${selectedVoice}",\n` : ''
    const escapedInstructions = instructions.replace(/\\/g, '\\\\').replace(/"/g, '\\"')
    const genericSession = `type: "session.update",\n    session: {\n${voiceSetting}      instructions: "${escapedInstructions}",\n      input_audio_format: "pcm16",\n      output_audio_format: "pcm16",\n    },`
    const jsonVoice = voices.length ? `"voice":"${selectedVoice}",` : ''
    if (selectedLanguage === 'python') return `# Python OpenAI Realtime Compatible Client\nimport asyncio\nimport websockets\nimport json\nimport base64\n\nasync def voice_session():\n    uri = "${url}"\n    headers = {"Authorization": "Bearer ${apiKey || 'YOUR_9ROUTER_KEY'}"}\n    async with websockets.connect(uri, extra_headers=headers) as ws:\n        await ws.send(json.dumps({\n            "type": "session.update",\n            "session": {\n${voices.length ? `                "voice": "${selectedVoice}",\n` : ''}                "instructions": "${escapedInstructions}",\n                "input_audio_format": "pcm16",\n                "output_audio_format": "pcm16",\n            }\n        }))\n        async for msg in ws:\n            event = json.loads(msg)\n            if event.get("type") == "response.audio_transcript.delta":\n                print(event["delta"], end="", flush=True)\n            elif event.get("type") == "response.audio.delta":\n                pcm_chunk = base64.b64decode(event["delta"])\n                # Play 24kHz 16-bit PCM audio chunk\n\nasyncio.run(voice_session())`
    if (selectedLanguage === 'cpp') return `// ESP32 / Arduino WebSocket Realtime Voice Client\n// Required: ArduinoWebsockets, ArduinoJson v7+\n#include <WiFi.h>\n#include <ArduinoWebsockets.h>\n#include <ArduinoJson.h>\nusing namespace websockets;\nWebsocketsClient client;\nconst char* ws_url = "${deviceUrl}";\n\nvoid onMessageCallback(WebsocketsMessage message) {\n  if (!message.isText()) return;\n  JsonDocument doc;\n  deserializeJson(doc, message.data());\n  const char* type = doc["type"];\n  if (strcmp(type, "response.audio_transcript.delta") == 0)\n    Serial.printf("AI: %s\\n", doc["delta"].as<const char*>());\n  else if (strcmp(type, "response.audio.delta") == 0) {\n    const char* base64_audio = doc["delta"];\n    // Decode base64 and write 24kHz PCM to the I2S speaker.\n  }\n}\n\nvoid setup() {\n  Serial.begin(115200);\n  WiFi.begin("YOUR_WIFI_SSID", "YOUR_WIFI_PASSWORD");\n  client.onMessage(onMessageCallback);\n  client.connect(ws_url);\n  client.send("{\\"type\\":\\"session.update\\",\\"session\\":{${jsonVoice}\\"instructions\\":\\"Answer briefly.\\",\\"input_audio_format\\":\\"pcm16\\",\\"output_audio_format\\":\\"pcm16\\"}}");\n}\nvoid loop() { client.poll(); /* Stream 16kHz PCM mic samples with sendBinary(). */ }`
    if (selectedLanguage === 'wscat') return `# Connect directly via wscat CLI\nwscat -c "${url}"\n\n# Send session initialization:\n{"type":"session.update","session":{"${jsonVoice}"instructions":"Answer briefly.","input_audio_format":"pcm16","output_audio_format":"pcm16"}}`
    return `// Node.js OpenAI Realtime Compatible Client\nimport WebSocket from "ws";\n\nconst WS_URL = "${url}";\nconst ws = new WebSocket(WS_URL, {\n  headers: { Authorization: "Bearer ${apiKey || 'YOUR_9ROUTER_KEY'}" },\n});\n\nws.on("open", () => {\n  console.log("Connected to 9Router Realtime Voice Gateway");\n  ws.send(JSON.stringify({\n    ${genericSession}\n  }));\n  // Stream raw 16-bit 16kHz PCM audio bytes with ws.send(pcm16Buffer).\n});\n\nws.on("message", (data, isBinary) => {\n  if (isBinary) return;\n  const event = JSON.parse(data.toString());\n  if (event.type === "response.audio_transcript.delta") process.stdout.write(event.delta);\n  else if (event.type === "response.audio.delta") {\n    const pcm24k = Buffer.from(event.delta, "base64");\n    // Play or pipe the 24kHz PCM audio to a speaker.\n  }\n});`
  })

  $effect(() => {
    if (messages.length && transcriptElement) {
      transcriptElement.scrollTo({ top: transcriptElement.scrollHeight, behavior: 'smooth' })
    }
  })

  function toPcm16(samples: Float32Array): ArrayBuffer {
    const buffer = new ArrayBuffer(samples.length * 2)
    const view = new DataView(buffer)
    for (let index = 0; index < samples.length; index++) {
      const sample = Math.max(-1, Math.min(1, samples[index]))
      view.setInt16(index * 2, sample < 0 ? sample * 0x8000 : sample * 0x7fff, true)
    }
    return buffer
  }

  function bufferToBase64(buffer: ArrayBuffer): string {
    const bytes = new Uint8Array(buffer)
    let binary = ''
    for (const byte of bytes) binary += String.fromCharCode(byte)
    return window.btoa(binary)
  }

  function stopSources() {
    for (const source of activeSources) {
      try { source.stop() } catch { /* Already stopped. */ }
    }
    activeSources = []
    nextPlayTime = 0
  }

  function playAudio(base64Audio: string) {
    try {
      audioContextOut ??= new AudioContext({ sampleRate: 24000 })
      const context = audioContextOut
      if (context.state === 'suspended') void context.resume()
      const binary = window.atob(base64Audio)
      const bytes = new Uint8Array(binary.length)
      for (let index = 0; index < binary.length; index++) bytes[index] = binary.charCodeAt(index)
      const pcm = new Int16Array(bytes.buffer)
      const audio = context.createBuffer(1, pcm.length, 24000)
      const samples = audio.getChannelData(0)
      for (let index = 0; index < pcm.length; index++) samples[index] = pcm[index] / 32768
      const source = context.createBufferSource()
      source.buffer = audio
      source.connect(context.destination)
      const startTime = Math.max(context.currentTime, nextPlayTime)
      source.start(startTime)
      nextPlayTime = startTime + audio.duration
      activeSources = [...activeSources, source]
      source.onended = () => {
        activeSources = activeSources.filter((playing) => playing !== source)
        if (activeSources.length === 0 && socket?.readyState === WebSocket.OPEN) connectionStatus = 'listening'
      }
      connectionStatus = 'speaking'
    } catch (error) {
      statusMessage = `Audio playback failed: ${error instanceof Error ? error.message : String(error)}`
    }
  }

  function handleServerEvent(message: { type?: string; delta?: string; error?: { message?: string } }) {
    if (message.type === 'response.audio.delta' && message.delta) {
      playAudio(message.delta)
    } else if (message.type === 'response.audio_transcript.delta' && message.delta) {
      const last = messages.at(-1)
      if (last?.role === 'assistant' && !last.isDone) {
        messages = [...messages.slice(0, -1), { ...last, text: last.text + message.delta }]
      } else {
        messages = [...messages, { id: ++messageId, role: 'assistant', text: message.delta, isDone: false }]
      }
    } else if (message.type === 'response.done') {
      const last = messages.at(-1)
      if (last?.role === 'assistant') messages = [...messages.slice(0, -1), { ...last, isDone: true }]
    } else if (message.type === 'input_audio_buffer.speech_started') {
      stopSources()
      if (audioContextOut) nextPlayTime = audioContextOut.currentTime
      connectionStatus = 'listening'
    } else if (message.type === 'error') {
      statusMessage = `Error: ${message.error?.message || 'Unknown error'}`
      connectionStatus = 'error'
    }
  }

  function stopCall() {
    calling = false
    if (volumeFrame) cancelAnimationFrame(volumeFrame)
    volumeFrame = 0
    if (processor) {
      try { processor.disconnect() } catch { /* Already disconnected. */ }
      processor.onaudioprocess = null
      processor = null
    }
    audioSource?.disconnect()
    audioSource = null
    analyser?.disconnect()
    analyser = null
    if (audioContextIn) {
      void audioContextIn.close().catch(() => {})
      audioContextIn = null
    }
    mediaStream?.getTracks().forEach((track) => track.stop())
    mediaStream = null
    stopSources()
    if (audioContextOut) {
      void audioContextOut.close().catch(() => {})
      audioContextOut = null
    }
    const current = socket
    socket = null
    if (current && current.readyState < WebSocket.CLOSING) current.close(1000, 'User ended call')
    connectionStatus = 'idle'
    statusMessage = ''
    micVolume = 0
  }

  async function startCall() {
    try {
      connectionStatus = 'connecting'
      statusMessage = 'Requesting microphone permission…'
      messages = []
      mediaStream = await navigator.mediaDevices.getUserMedia({
        audio: { channelCount: 1, sampleRate: 16000, echoCancellation: true, noiseSuppression: true, autoGainControl: true },
      })
      if (!mounted) {
        mediaStream.getTracks().forEach((track) => track.stop())
        mediaStream = null
        return
      }
      audioContextIn = new AudioContext({ sampleRate: 16000 })
      audioSource = audioContextIn.createMediaStreamSource(mediaStream)
      analyser = audioContextIn.createAnalyser()
      analyser.fftSize = 256
      audioSource.connect(analyser)
      processor = audioContextIn.createScriptProcessor(2048, 1, 1)
      const samples = new Uint8Array(analyser.frequencyBinCount)
      const readVolume = () => {
        if (!processor || !analyser) return
        analyser.getByteFrequencyData(samples)
        const average = samples.reduce((sum, sample) => sum + sample, 0) / samples.length
        micVolume = Math.min(100, Math.round(average * 1.5))
        volumeFrame = requestAnimationFrame(readVolume)
      }
      volumeFrame = requestAnimationFrame(readVolume)
      statusMessage = 'Connecting to 9Router Voice Gateway…'
      const ws = new WebSocket(wsUrl)
      socket = ws
      ws.onopen = () => {
        if (socket !== ws) return
        connectionStatus = 'connected'
        statusMessage = 'Connected. Say something or listen…'
        calling = true
        ws.send(JSON.stringify({
          type: 'session.update',
          session: {
            ...(voices.length ? { voice: selectedVoice } : {}),
            instructions,
            input_audio_format: 'pcm16',
            output_audio_format: 'pcm16',
          },
        }))
        const currentProcessor = processor
        if (!currentProcessor || !audioSource || !audioContextIn) return
        currentProcessor.onaudioprocess = (event) => {
          if (ws.readyState !== WebSocket.OPEN) return
          ws.send(JSON.stringify({ type: 'input_audio_buffer.append', audio: bufferToBase64(toPcm16(event.inputBuffer.getChannelData(0))) }))
        }
        audioSource.connect(currentProcessor)
        currentProcessor.connect(audioContextIn.destination)
      }
      ws.onmessage = (event: MessageEvent<string>) => {
        try { handleServerEvent(JSON.parse(event.data) as { type?: string; delta?: string; error?: { message?: string } }) } catch { /* Ignore malformed frames. */ }
      }
      ws.onerror = () => {
        connectionStatus = 'error'
        statusMessage = 'WebSocket connection error. Check the server or API key.'
      }
      ws.onclose = (event) => {
        if (socket !== ws) return
        stopCall()
        if (event.reason) statusMessage = `Connection closed: ${event.reason}`
      }
    } catch (error) {
      stopCall()
      connectionStatus = 'error'
      statusMessage = `Unable to start voice call: ${error instanceof Error ? error.message : String(error)}`
    }
  }

  async function copy(value: string, target: 'url' | 'code') {
    await copyToClipboard(value)
    if (target === 'url') copiedWs = true
    else copiedCode = true
    setTimeout(() => {
      if (target === 'url') copiedWs = false
      else copiedCode = false
    }, 1800)
  }

  onMount(() => {
    mounted = true
    localEndpoint = window.location.origin
    apiKey = getStoredAPIKey() || apiKeys.find((key) => key.isActive !== 0 && key.key)?.key || ''
    api.getTunnelStatus().then((status) => {
      tunnelEndpoint = status.tunnel?.publicUrl || status.tunnel?.tunnelUrl || status.tailscale?.tunnelUrl || ''
    }).catch(() => {})
    return () => {
      mounted = false
      stopCall()
    }
  })
</script>

<Card class="shadow-[var(--shadow-warm)]">
  <div class="flex flex-col gap-5">
    <header class="flex flex-col gap-3 border-b border-border/40 pb-4 sm:flex-row sm:items-start sm:justify-between">
      <div>
        <div class="flex flex-wrap items-center gap-2">
          <span class="material-symbols-outlined text-[22px] text-primary">phone_in_talk</span>
          <h2 class="text-base font-semibold text-text-main">Realtime Voice Agent</h2>
          <span class="rounded-full bg-primary/10 px-2 py-0.5 text-[10px] font-semibold tracking-wide text-primary">OPENAI REALTIME PROTOCOL</span>
          <span class="rounded-full border border-border px-2 py-0.5 text-[10px] font-semibold text-text-muted">{providerName}</span>
        </div>
        <p class="mt-1 text-xs text-text-muted">Bidirectional live voice conversation over the 9router WebSocket gateway.</p>
      </div>
      <span class="inline-flex w-fit items-center gap-1.5 rounded-full border px-2.5 py-1 text-[11px] font-semibold {connectionStatus === 'error' ? 'border-red-500/30 bg-red-500/10 text-red-500' : connectionStatus === 'connected' || connectionStatus === 'listening' ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-600 dark:text-emerald-400' : connectionStatus === 'speaking' ? 'border-primary/30 bg-primary/10 text-primary animate-pulse' : connectionStatus === 'connecting' ? 'border-amber-500/30 bg-amber-500/10 text-amber-600' : 'border-border bg-surface-2 text-text-muted'}" aria-live="polite">
        <span class="size-1.5 rounded-full {connectionStatus === 'error' ? 'bg-red-500' : connectionStatus === 'connected' || connectionStatus === 'listening' ? 'bg-emerald-500' : connectionStatus === 'speaking' ? 'bg-primary' : connectionStatus === 'connecting' ? 'bg-amber-500 animate-pulse' : 'bg-border'}"></span>{badgeText}
      </span>
    </header>

    <div class="grid grid-cols-1 gap-4 rounded-[14px] border border-border/60 bg-surface-2/40 p-4 md:grid-cols-2">
      {#if isDeepgram}
        <label class="flex min-w-0 flex-col gap-2 text-xs font-semibold uppercase tracking-wider text-text-muted">
          LLM (think) · Provider
          <select aria-label="LLM think provider" bind:value={selectedThinkProvider} disabled={calling} onchange={() => {
            const picked = thinkProviders.find((provider) => provider.type === selectedThinkProvider)
            selectedThinkModel = picked?.models?.[0]?.id || ''
          }} class="rounded-[10px] border border-border bg-surface px-3 py-2 text-sm normal-case tracking-normal text-text-main focus:border-primary focus:outline-none disabled:opacity-50">
            {#each (thinkProviders.length ? thinkProviders : thinkDefaultProvider ? [{ type: thinkDefaultProvider, name: thinkDefaultProvider }] : []) as option (option.type)}
              <option value={option.type}>{option.name || option.type}</option>
            {/each}
          </select>
        </label>
        <label class="flex min-w-0 flex-col gap-2 text-xs font-semibold uppercase tracking-wider text-text-muted">
          LLM (think) · Model
          <select aria-label="LLM think model" bind:value={selectedThinkModel} disabled={calling} class="rounded-[10px] border border-border bg-surface px-3 py-2 text-sm normal-case tracking-normal text-text-main focus:border-primary focus:outline-none disabled:opacity-50">
            {#each (thinkProviders.find((provider) => provider.type === selectedThinkProvider)?.models || (thinkDefaultModel ? [{ id: thinkDefaultModel }] : [])) as model (model.id)}<option value={model.id}>{model.name || model.id}</option>{/each}
            <option value="__custom__">Custom…</option>
          </select>
          {#if selectedThinkModel === '__custom__'}
            <input aria-label="Custom LLM model ID" bind:value={customThinkModel} disabled={calling} placeholder="Enter a model ID" class="rounded-[10px] border border-border bg-surface px-3 py-2 text-sm normal-case tracking-normal text-text-main focus:border-primary focus:outline-none disabled:opacity-50" />
          {/if}
        </label>
        <label class="md:col-span-2 flex min-w-0 flex-col gap-2 text-xs font-semibold uppercase tracking-wider text-text-muted">
          Greeting (optional)
          <input aria-label="Agent greeting" bind:value={greeting} disabled={calling} placeholder="The first thing the agent says…" class="rounded-[10px] border border-border bg-surface px-3 py-2 text-sm normal-case tracking-normal text-text-main focus:border-primary focus:outline-none disabled:opacity-50" />
        </label>
        <label class="md:col-span-2 flex min-w-0 flex-col gap-2 text-xs font-semibold uppercase tracking-wider text-text-muted">
          Agent prompt
          <textarea aria-label="Agent prompt" bind:value={instructions} disabled={calling} rows="2" class="resize-y rounded-[10px] border border-border bg-surface px-3 py-2 text-sm normal-case tracking-normal text-text-main focus:border-primary focus:outline-none disabled:opacity-50" placeholder="Prompt for the voice agent…"></textarea>
        </label>
        <label class="flex min-w-0 flex-col gap-2 text-xs font-semibold uppercase tracking-wider text-text-muted">
          Voice (speak)
          <select aria-label="Speak voice" bind:value={selectedSpeak} disabled={calling} class="rounded-[10px] border border-border bg-surface px-3 py-2 text-sm normal-case tracking-normal text-text-main focus:border-primary focus:outline-none disabled:opacity-50">
            {#each (speakOptions.length ? speakOptions : realtimeConfig.speak?.provider ? [{ value: `${realtimeConfig.speak.provider.type}/${realtimeConfig.speak.provider.model}`, label: `${realtimeConfig.speak.provider.type}/${realtimeConfig.speak.provider.model}` }] : []) as option (option.value)}<option value={option.value}>{option.label}</option>{/each}
          </select>
          {#if selectedSpeak.endsWith('/custom:')}<input aria-label="Custom voice ID" bind:value={customSpeakId} disabled={calling} placeholder="Enter a voice ID" class="rounded-[10px] border border-border bg-surface px-3 py-2 text-sm normal-case tracking-normal text-text-main focus:border-primary focus:outline-none disabled:opacity-50" />{/if}
        </label>
        <label class="flex min-w-0 flex-col gap-2 text-xs font-semibold uppercase tracking-wider text-text-muted">
          Listen (STT)
          <select aria-label="Listen speech-to-text model" bind:value={selectedListen} disabled={calling} class="rounded-[10px] border border-border bg-surface px-3 py-2 text-sm normal-case tracking-normal text-text-main focus:border-primary focus:outline-none disabled:opacity-50">
            {#each (listenOptions.length ? listenOptions : realtimeConfig.listen?.provider ? [{ value: `${realtimeConfig.listen.provider.type}/${realtimeConfig.listen.provider.model}`, label: `${realtimeConfig.listen.provider.type}/${realtimeConfig.listen.provider.model}` }] : []) as option (option.value)}<option value={option.value}>{option.label}</option>{/each}
          </select>
          {#if selectedListen.endsWith('/custom:')}<input aria-label="Custom listen model ID" bind:value={customListenId} disabled={calling} placeholder="Enter a model ID" class="rounded-[10px] border border-border bg-surface px-3 py-2 text-sm normal-case tracking-normal text-text-main focus:border-primary focus:outline-none disabled:opacity-50" />{/if}
        </label>
      {:else if voices.length}
        <label class="flex min-w-0 flex-col gap-2 text-xs font-semibold uppercase tracking-wider text-text-muted">
          AI Voice
          <select aria-label="AI voice" bind:value={selectedVoice} disabled={calling} class="rounded-[10px] border border-border bg-surface px-3 py-2 text-sm normal-case tracking-normal text-text-main focus:border-primary focus:outline-none disabled:opacity-50">
            {#each voices as voice (voice.id)}<option value={voice.id}>{voice.label}</option>{/each}
          </select>
        </label>
        <label class="md:col-span-2 flex min-w-0 flex-col gap-2 text-xs font-semibold uppercase tracking-wider text-text-muted">
          System Instructions
          <textarea aria-label="System instructions" bind:value={instructions} disabled={calling} rows="2" class="resize-y rounded-[10px] border border-border bg-surface px-3 py-2 text-sm normal-case tracking-normal text-text-main focus:border-primary focus:outline-none disabled:opacity-50" placeholder="Prompt for the voice agent…"></textarea>
        </label>
      {/if}
      <label class="flex min-w-0 flex-col gap-2 text-xs font-semibold uppercase tracking-wider text-text-muted">
        API key
        <input aria-label="API key" type="password" autocomplete="off" bind:value={apiKey} disabled={calling} placeholder="No client key configured" class="rounded-[10px] border border-border bg-surface px-3 py-2 text-sm normal-case tracking-normal text-text-main focus:border-primary focus:outline-none disabled:opacity-50" />
      </label>
      <div class="flex min-w-0 flex-col gap-2 text-xs font-semibold uppercase tracking-wider text-text-muted">
        <div class="flex items-center justify-between gap-2">
          <label for="realtime-endpoint">Connection endpoint</label>
          {#if tunnelEndpoint}
            <label class="inline-flex items-center gap-2 text-xs font-medium normal-case tracking-normal text-text-muted">
              <input id="realtime-tunnel" type="checkbox" bind:checked={useTunnel} disabled={calling} class="size-4 accent-primary" />Use Public Tunnel
            </label>
          {/if}
        </div>
        <input id="realtime-endpoint" aria-label="Connection endpoint URL" readonly value={wsUrl} class="w-full min-w-0 rounded-[10px] border border-border bg-sidebar px-3 py-2 font-mono text-[11px] text-text-muted focus:border-primary focus:outline-none" />
        <button type="button" onclick={() => copy(wsUrl, 'url')} class="inline-flex w-fit items-center gap-1 text-xs font-medium normal-case tracking-normal text-primary hover:underline">
          <span class="material-symbols-outlined text-[14px]">{copiedWs ? 'check' : 'content_copy'}</span>{copiedWs ? 'Copied URL' : 'Copy WebSocket URL'}
        </button>
      </div>
    </div>

    <div class="relative flex flex-col items-center justify-center gap-4 overflow-hidden rounded-[14px] border border-border/80 bg-surface-2/40 px-4 py-7">
      <div class="relative flex size-28 items-center justify-center" aria-hidden="true">
        {#if calling}
          <span class="absolute inset-0 rounded-full transition-transform duration-150 {connectionStatus === 'speaking' ? 'animate-ping bg-primary/20' : 'bg-emerald-500/20'}" style:transform={`scale(${1 + micVolume / 200})`}></span>
          <span class="absolute inset-2 rounded-full opacity-60 transition-transform duration-100 {connectionStatus === 'speaking' ? 'bg-primary/30' : 'bg-emerald-500/30'}" style:transform={`scale(${1 + micVolume / 300})`}></span>
        {/if}
        <span class="relative flex size-20 items-center justify-center rounded-full text-white shadow-lg transition-colors {calling ? connectionStatus === 'speaking' ? 'bg-primary shadow-primary/30' : 'bg-emerald-600 shadow-emerald-500/30' : 'bg-text-muted/30'}">
          <span class="material-symbols-outlined text-3xl">{calling ? connectionStatus === 'speaking' ? 'volume_up' : 'mic' : 'phone_in_talk'}</span>
        </span>
      </div>
      <div class="text-center" aria-live="polite">
        <p class="text-sm font-semibold text-text-main">{calling ? statusMessage || (connectionStatus === 'speaking' ? 'AI is speaking…' : 'Listening to you…') : 'Ready to start a voice conversation'}</p>
        <p class="mt-1 text-xs text-text-muted">{calling ? 'Speak into your microphone. You can interrupt anytime.' : `Connect your microphone and talk to ${providerName}.`}</p>
      </div>
      {#if !calling}
        <button type="button" onclick={startCall} disabled={connectionStatus === 'connecting' || !realtimeModel} class="inline-flex items-center gap-2 rounded-[10px] bg-primary px-5 py-2.5 text-sm font-semibold text-white transition-colors hover:bg-primary-hover disabled:cursor-not-allowed disabled:opacity-50">
          <span class="material-symbols-outlined text-[18px]">mic</span>Start Voice Conversation
        </button>
      {:else}
        <button type="button" onclick={stopCall} class="inline-flex items-center gap-2 rounded-[10px] border border-red-500/25 bg-red-500/10 px-5 py-2.5 text-sm font-semibold text-red-600 transition-colors hover:bg-red-500/15 dark:text-red-400">
          <span class="material-symbols-outlined text-[18px]">call_end</span>End Voice Call
        </button>
      {/if}
    </div>

    <section class="flex flex-col gap-2" aria-label="Live transcript">
      <div class="flex items-center justify-between">
        <h3 class="text-xs font-semibold uppercase tracking-wider text-text-muted">Live Transcript</h3>
        {#if messages.length}<button type="button" onclick={() => (messages = [])} class="rounded px-2 py-1 text-xs text-text-muted hover:bg-surface-2 hover:text-text-main">Clear transcript</button>{/if}
      </div>
      <div bind:this={transcriptElement} class="flex h-44 flex-col gap-2 overflow-y-auto rounded-[14px] border border-border/80 bg-surface p-3 text-xs" aria-live="polite" aria-relevant="additions text">
        {#if messages.length === 0}
          <div class="flex flex-1 items-center justify-center text-center italic text-text-muted">Spoken conversation and AI responses will appear here in real time…</div>
        {:else}
          {#each messages as message (message.id)}
            <div class="flex flex-col gap-1 {message.role === 'assistant' ? 'items-start' : 'items-end'}">
              <div class="max-w-[85%] rounded-[10px] border px-3 py-2 leading-relaxed {message.role === 'assistant' ? 'border-border bg-surface-2 text-text-main' : 'border-primary bg-primary text-white'}">
                <p class="mb-0.5 text-[10px] font-semibold uppercase opacity-75">{message.role === 'assistant' ? providerName : 'You'}</p>
                <p>{message.text}</p>
              </div>
            </div>
          {/each}
        {/if}
      </div>
    </section>

    <section class="flex flex-col gap-2 border-t border-border/60 pt-4" aria-label="Developer integration snippets">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <h3 class="text-xs font-semibold uppercase tracking-wider text-text-muted">Developer Integration</h3>
        <div class="flex flex-wrap gap-1 rounded-[10px] border border-border bg-surface-2 p-1" role="tablist" aria-label="Code language">
          {#each [{ id: 'nodejs', label: 'Node.js' }, { id: 'python', label: 'Python' }, { id: 'cpp', label: 'C++ ESP32' }, { id: 'wscat', label: 'wscat' }] as tab (tab.id)}
            <button type="button" role="tab" aria-selected={selectedLanguage === tab.id} onclick={() => (selectedLanguage = tab.id as SnippetLanguage)} class="rounded-md px-2.5 py-1 text-xs font-medium transition-colors {selectedLanguage === tab.id ? 'bg-primary text-white shadow-sm' : 'text-text-muted hover:text-text-main'}">{tab.label}</button>
          {/each}
        </div>
      </div>
      <div class="relative">
        <button type="button" onclick={() => copy(currentSnippet, 'code')} class="absolute right-2 top-2 z-10 inline-flex items-center gap-1 rounded-md border border-border bg-surface px-2.5 py-1 text-[11px] font-medium text-text-muted shadow-sm transition-colors hover:bg-surface-2 hover:text-text-main">
          <span class="material-symbols-outlined text-[13px]">{copiedCode ? 'check' : 'content_copy'}</span>{copiedCode ? 'Copied' : 'Copy code'}
        </button>
        <pre class="max-h-96 overflow-x-auto whitespace-pre-wrap break-all rounded-[10px] border border-border bg-sidebar p-3.5 pt-10 font-mono text-[11px] leading-relaxed text-text-main">{currentSnippet}</pre>
      </div>
    </section>
  </div>
</Card>
