//! Negotiated message IPC: a u64 logical length, finite 64 KiB chunks,
//! and an explicit sequence-checked terminator. No u32 message-length cast.

use super::*;

pub(super) const MESSAGE_START: u8 = 71;
pub(super) const MESSAGE_CHUNK: u8 = 72;
pub(super) const MESSAGE_FINISH: u8 = 73;
pub(super) const SEND_START: u8 = 85;
pub(super) const SEND_CHUNK: u8 = 86;
pub(super) const SEND_FINISH: u8 = 87;
pub(super) const CHUNK_SIZE: usize = 64 * 1024;

pub(super) async fn write_message<W: AsyncWrite + Unpin>(writer: &mut W, in_frame: &mut bool, data: &[u8], duration: Duration) -> Result<()> {
    if data.len() < 9 { return Err("ws_command_invalid"); }
    let total = u64::try_from(data.len() - 9).map_err(|_| "ws_message_limit")?;
    let mut start = [0_u8; 17];
    start[..9].copy_from_slice(&data[..9]);
    start[9..].copy_from_slice(&total.to_be_bytes());
    write_packet(writer, in_frame, MESSAGE_START, &start, duration).await?;
    for chunk in data[9..].chunks(CHUNK_SIZE) {
        write_packet(writer, in_frame, MESSAGE_CHUNK, chunk, duration).await?;
    }
    write_packet(writer, in_frame, MESSAGE_FINISH, &data[..8], duration).await
}

// The wait between commands has no timer. A partial finite IPC packet retains
// its inactivity budget; increasing the logical payload does not create a
// fixed total-transfer deadline.
async fn packet<R: AsyncRead + Unpin>(reader: &mut R, accepted: &AtomicBool, duration: Duration) -> Result<(u8, Vec<u8>)> {
    let kind = reader.read_u8().await.map_err(|_| "cancelled")?;
    timeout(duration, async {
        let length = reader.read_u32().await.map_err(|_| "ws_ipc_read")? as usize;
        if kind == CANCEL && length == 0 { return Err("cancelled"); }
        if !accepted.load(Ordering::Acquire) { return Err("ws_before_handshake"); }
        let valid = match kind {
            SEND_START => length == 17,
            SEND_CHUNK => (1..=CHUNK_SIZE).contains(&length),
            SEND_FINISH => length == 8,
            SEND_CLOSE => length <= MAX_HEAD,
            _ => false,
        };
        if !valid { return Err("ws_command_invalid"); }
        let mut data = vec![0; length];
        reader.read_exact(&mut data).await.map_err(|_| "ws_ipc_read")?;
        Ok((kind, data))
    }).await.map_err(|_| "ws_ipc_read_timeout")?
}

pub(super) async fn input<R: AsyncRead + Unpin>(reader: &mut R, commands: mpsc::Sender<Command>, accepted: Arc<AtomicBool>, busy: Arc<AtomicBool>, duration: Duration) -> Result<()> {
    let mut next_sequence = 1_u64;
    loop {
        let (kind, data) = packet(reader, &accepted, duration).await?;
        if busy.swap(true, Ordering::AcqRel) { return Err("ws_command_invalid"); }
        let command = if kind == SEND_START {
            let sequence = u64::from_be_bytes(data[..8].try_into().map_err(|_| "ws_sequence")?);
            if sequence != next_sequence { return Err("ws_sequence"); }
            next_sequence = next_sequence.checked_add(1).ok_or("ws_sequence")?;
            let message_kind = data[8];
            if message_kind != 1 && message_kind != 2 { return Err("ws_message_kind"); }
            let total = usize::try_from(u64::from_be_bytes(data[9..].try_into().map_err(|_| "ws_command_invalid")?)).map_err(|_| "ws_message_limit")?;
            // Vec is bounded by isize::MAX by the platform, not policy. Do not
            // allocate from a declared total: grow only for bytes received.
            if total > isize::MAX as usize { return Err("ws_message_limit"); }
            let mut payload = Vec::new();
            loop {
                let (kind, chunk) = packet(reader, &accepted, duration).await?;
                match kind {
                    SEND_CHUNK => {
                        let new_length = payload.len().checked_add(chunk.len()).ok_or("ws_command_invalid")?;
                        if new_length > total { return Err("ws_command_invalid"); }
                        payload.try_reserve(chunk.len()).map_err(|_| "ws_message_allocation")?;
                        payload.extend_from_slice(&chunk);
                    }
                    SEND_FINISH => {
                        if chunk != sequence.to_be_bytes() || payload.len() != total { return Err("ws_command_invalid"); }
                        break;
                    }
                    _ => return Err("ws_command_invalid"),
                }
            }
            let message = if message_kind == 1 {
                Message::Text(String::from_utf8(payload).map_err(|_| "ws_text_utf8")?.into())
            } else { Message::Binary(payload.into()) };
            Command::Send(sequence, message)
        } else if kind == SEND_CLOSE {
            let close: CloseHead = serde_json::from_slice(&data).map_err(|_| "ws_close_invalid")?;
            if close.reason.len() > 123 || close.code.is_none() && !close.reason.is_empty() { return Err("ws_close_invalid"); }
            let frame = match close.code {
                None => None,
                Some(code) => {
                    let code = tungstenite::protocol::frame::coding::CloseCode::from(code);
                    if !code.is_allowed() { return Err("ws_close_invalid"); }
                    Some(CloseFrame { code, reason: close.reason.into() })
                }
            };
            Command::Close(frame)
        } else { return Err("ws_command_invalid"); };
        commands.try_send(command).map_err(|_| "ws_command_capacity")?;
    }
}

#[cfg(test)]
#[path = "websocket_v2_tests.rs"]
mod tests;
