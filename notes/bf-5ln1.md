# Whisper Transcription Opt-In Verification (Bead bf-5ln1)

## Status
**COMPLETED** - Feature fully implemented in commits `a8441a5` and `6590bab`

## Overview
Implemented opt-in feature to send Whisper transcriptions back to the user for verification before dispatching to Claude, as specified in Plan §3.2 (Voice/Audio Support).

## Implementation Details

### Database Schema (Migration 22)
- Added `transcript_verify` column to `groups` table (INTEGER, default 0)
- Group-level configuration flag to enable/disable verification per group

### State Management (`internal/bridge/state.go`)
- `Group.TranscriptVerify bool` field (line 45)
- `scanGroup()` converts INTEGER to bool (line 545)
- Migration applied on startup automatically

### Session Manager (`internal/bridge/session_manager.go`)

#### Pending Transcript Storage
- `pendingTranscripts map[topicKey]map[int64]string` (line 404)
- Stores transcripts awaiting user approval, keyed by (chatID, threadID, messageID)

#### Key Methods
1. **`StorePendingTranscript()`** (lines 2765-2773)
   - Stores transcript in memory map after Whisper transcription completes
   - Thread-safe with mutex protection

2. **`GetPendingTranscript()`** (lines 2777-2786)
   - Retrieves stored transcript by messageID
   - Returns (transcript, true) if found, ("", false) otherwise

3. **`ClearPendingTranscript()`** (lines 2789-2796)
   - Removes transcript from storage after processing
   - Called on approval, edit, or timeout

4. **`SubmitApprovedTranscript()`** (lines 2800-2837)
   - Creates synthetic text update with approved transcript
   - Re-processes through SessionManager as if user typed the text
   - Clears pending transcript after submission

#### Verification Flow (lines 862-907)
1. After `processAudio()` returns transcription, check `group.TranscriptVerify`
2. If enabled:
   - Store transcript with `StorePendingTranscript()`
   - Send verification prompt with inline keyboard
   - Clean up temp files and return early (wait for callback)
3. If disabled:
   - Continue normal flow (prepend transcription to prompt)

### Callback Handler (`internal/bridge/callback_handler.go`)

#### `handleTranscriptApproval()` (lines 131-161)
- Parses callback data: `"approve_transcript:chatID:threadID:messageID"`
- Validates chat match
- Retrieves pending transcript
- Calls `SubmitApprovedTranscript()` to dispatch to Claude
- Returns confirmation message

#### `handleTranscriptEdit()` (lines 165-205)
- Parses callback data: `"edit_transcript:chatID:threadID:messageID"`
- Clears pending transcript (user will send edited text manually)
- Sends instructions message with current transcript for copy/edit
- Returns "Edit mode activated" confirmation

### Sender (`internal/bridge/sender.go`)

#### `SendTranscriptVerifyPrompt()` (lines 440-475)
- Formats transcript for display (truncates to 500 chars if needed)
- Builds message with transcript text and inline keyboard
- Keyboard buttons:
  - "📤 Send to Claude" → `approve_transcript:chatID:threadID:messageID`
  - "✏️ Edit first" → `edit_transcript:chatID:threadID:messageID`
- Sends message to topic, returns messageID

## Usage

### Enable Verification for a Group
```sql
UPDATE groups SET transcript_verify = 1 WHERE chat_id = ?;
```

### Disable Verification
```sql
UPDATE groups SET transcript_verify = 0 WHERE chat_id = ?;
```

### User Flow
1. User sends voice/audio message
2. Whisper transcribes audio
3. Bot sends verification prompt with transcription preview
4. User chooses:
   - **"Send to Claude"**: Transcript dispatched as user message
   - **"Edit first"**: Bot clears pending state, user sends corrected text

## Testing
- Feature works with all audio types: voice messages, audio files, video with audio
- Pending transcripts stored in-memory only (cleared on bridge restart)
- Callback validation prevents cross-chat transcript approval
- Thread-safe concurrent operations

## Files Modified
- `internal/bridge/state.go` - Group struct, migration 22, scanGroup
- `internal/bridge/session_manager.go` - Verification flow, pending transcript methods
- `internal/bridge/callback_handler.go` - Approval and edit handlers
- `internal/bridge/sender.go` - Verification prompt sender
