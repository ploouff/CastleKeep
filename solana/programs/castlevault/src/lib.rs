//! CastleVault for Solana: a design sketch, NOT compiled or tested.
//!
//! Same model as the Ethereum contract:
//! - The user locks SOL in a program-owned vault account (a PDA).
//! - Every trade produces a state (nonce, user_amount) that both sides sign.
//! - `settle` pays out immediately when both parties sign the transaction.
//! - `request_exit` starts a window; during it `submit_state` accepts only a
//!   state with a higher nonce than any submitted before.
//! - `finalize` pays out the newest submitted state after the window, or
//!   everything to the user if nobody submitted one.
//!
//! Solana programs can't run signature checks on arbitrary messages cheaply
//! by themselves. Instead, the transaction includes calls to Solana's built-in
//! Ed25519 signature-verification program, and this program checks (through
//! the Instructions sysvar) that those calls verified the right keys and the
//! right message. That check is the most security-critical code here and the
//! part most in need of review and tests.

use anchor_lang::prelude::*;
use anchor_lang::solana_program::{ed25519_program, sysvar::instructions as ix_sysvar};

declare_id!("Cast1eVau1t11111111111111111111111111111111"); // placeholder: replace after `anchor keys list`

#[program]
pub mod castlevault {
    use super::*;

    /// Lock `amount` lamports in a new vault shared with `exchange`.
    pub fn open(ctx: Context<Open>, vault_id: u64, exchange: Pubkey, exit_delay: i64, amount: u64) -> Result<()> {
        require!(amount > 0, CastleError::NothingLocked);
        require!(exit_delay >= 60, CastleError::DelayTooShort);
        require_keys_neq!(exchange, ctx.accounts.user.key(), CastleError::BadExchange);

        let v = &mut ctx.accounts.vault;
        v.user = ctx.accounts.user.key();
        v.exchange = exchange;
        v.vault_id = vault_id;
        v.amount = amount;
        v.exit_delay = exit_delay;
        v.exit_start = 0;
        v.has_best = false;
        v.best_nonce = 0;
        v.best_user = 0;
        v.closed = false;
        v.bump = ctx.bumps.vault;

        // Move the user's SOL into the vault account.
        anchor_lang::system_program::transfer(
            CpiContext::new(
                ctx.accounts.system_program.to_account_info(),
                anchor_lang::system_program::Transfer {
                    from: ctx.accounts.user.to_account_info(),
                    to: ctx.accounts.vault.to_account_info(),
                },
            ),
            amount,
        )?;
        Ok(())
    }

    /// Cooperative close. On Solana both parties simply co-sign the transaction,
    /// so no separate state signatures are needed here.
    pub fn settle(ctx: Context<Settle>, user_amount: u64) -> Result<()> {
        let v = &mut ctx.accounts.vault;
        require!(!v.closed, CastleError::Closed);
        require!(v.exit_start == 0, CastleError::ExitInProgress);
        require!(user_amount <= v.amount, CastleError::SplitTooLarge);
        pay_out(v, &ctx.accounts.user_wallet, &ctx.accounts.exchange_wallet, user_amount)
    }

    /// Either party starts the exit window.
    pub fn request_exit(ctx: Context<RequestExit>) -> Result<()> {
        let v = &mut ctx.accounts.vault;
        let who = ctx.accounts.caller.key();
        require!(who == v.user || who == v.exchange, CastleError::NotAParty);
        require!(!v.closed, CastleError::Closed);
        require!(v.exit_start == 0, CastleError::ExitAlreadyRequested);
        v.exit_start = Clock::get()?.unix_timestamp;
        Ok(())
    }

    /// Submit a state both sides signed. The transaction must include two
    /// Ed25519 verification instructions (one per signer) for `state_message`.
    pub fn submit_state(ctx: Context<SubmitState>, nonce: u64, user_amount: u64) -> Result<()> {
        let now = Clock::get()?.unix_timestamp;
        let v = &mut ctx.accounts.vault;
        require!(!v.closed, CastleError::Closed);
        require!(v.exit_start != 0, CastleError::NoExit);
        require!(now < v.exit_start + v.exit_delay, CastleError::WindowEnded);
        require!(!v.has_best || nonce > v.best_nonce, CastleError::NewerStateExists);
        require!(user_amount <= v.amount, CastleError::SplitTooLarge);

        let msg = state_message(&ctx.accounts.vault.key(), v.vault_id, nonce, user_amount);
        let ixs = &ctx.accounts.instructions;
        require!(ed25519_verified(ixs, &v.user, &msg)?, CastleError::MissingUserSignature);
        require!(ed25519_verified(ixs, &v.exchange, &msg)?, CastleError::MissingExchangeSignature);

        v.has_best = true;
        v.best_nonce = nonce;
        v.best_user = user_amount;
        Ok(())
    }

    /// After the window: pay the newest submitted state, or everything to the user.
    pub fn finalize(ctx: Context<Finalize>) -> Result<()> {
        let now = Clock::get()?.unix_timestamp;
        let v = &mut ctx.accounts.vault;
        require!(!v.closed, CastleError::Closed);
        require!(v.exit_start != 0, CastleError::NoExit);
        require!(now >= v.exit_start + v.exit_delay, CastleError::WindowRunning);
        let user_amount = if v.has_best { v.best_user } else { v.amount };
        pay_out(v, &ctx.accounts.user_wallet, &ctx.accounts.exchange_wallet, user_amount)
    }
}

// ------------------------------------------------------------------ helpers

/// The bytes both parties sign for a state.
pub fn state_message(vault: &Pubkey, vault_id: u64, nonce: u64, user_amount: u64) -> Vec<u8> {
    let mut m = b"castlekeep-state".to_vec();
    m.extend_from_slice(vault.as_ref());
    m.extend_from_slice(&vault_id.to_le_bytes());
    m.extend_from_slice(&nonce.to_le_bytes());
    m.extend_from_slice(&user_amount.to_le_bytes());
    m
}

/// True if some instruction in this transaction is an Ed25519 program call that
/// verified `signer` over exactly `msg`, with all data inside that instruction.
///
/// Ed25519 instruction data layout (one signature):
///   [0] num_signatures (u8) [1] padding
///   [2..16] offsets struct: sig_offset, sig_ix_index, pubkey_offset,
///           pubkey_ix_index, msg_offset, msg_size, msg_ix_index (u16 each)
/// The *_ix_index fields must be u16::MAX, meaning "this same instruction";
/// otherwise an attacker could point them at data elsewhere.
fn ed25519_verified(ixs: &AccountInfo, signer: &Pubkey, msg: &[u8]) -> Result<bool> {
    let mut i = 0usize;
    while let Ok(ix) = ix_sysvar::load_instruction_at_checked(i, ixs) {
        i += 1;
        if ix.program_id != ed25519_program::ID || ix.data.len() < 16 || ix.data[0] != 1 {
            continue;
        }
        let d = &ix.data;
        let u16_at = |o: usize| u16::from_le_bytes([d[o], d[o + 1]]) as usize;
        let (sig_ix, pk_off, pk_ix, msg_off, msg_len, msg_ix) =
            (u16_at(4), u16_at(6), u16_at(8), u16_at(10), u16_at(12), u16_at(14));
        if sig_ix != u16::MAX as usize || pk_ix != u16::MAX as usize || msg_ix != u16::MAX as usize {
            continue;
        }
        if pk_off + 32 > d.len() || msg_off + msg_len > d.len() {
            continue;
        }
        if &d[pk_off..pk_off + 32] == signer.as_ref() && &d[msg_off..msg_off + msg_len] == msg {
            return Ok(true); // the runtime already rejected the tx if the signature was invalid
        }
    }
    Ok(false)
}

fn pay_out<'info>(
    v: &mut Account<'info, Vault>,
    user_wallet: &AccountInfo<'info>,
    exchange_wallet: &AccountInfo<'info>,
    user_amount: u64,
) -> Result<()> {
    require_keys_eq!(user_wallet.key(), v.user, CastleError::WrongWallet);
    require_keys_eq!(exchange_wallet.key(), v.exchange, CastleError::WrongWallet);
    v.closed = true;
    let exchange_amount = v.amount - user_amount;
    let vault_info = v.to_account_info();
    **vault_info.try_borrow_mut_lamports()? -= v.amount;
    **user_wallet.try_borrow_mut_lamports()? += user_amount;
    **exchange_wallet.try_borrow_mut_lamports()? += exchange_amount;
    Ok(())
}

// ------------------------------------------------------------------ accounts

#[account]
#[derive(InitSpace)]
pub struct Vault {
    pub user: Pubkey,
    pub exchange: Pubkey,
    pub vault_id: u64,
    pub amount: u64,
    pub exit_delay: i64,
    pub exit_start: i64,
    pub best_nonce: u64,
    pub best_user: u64,
    pub has_best: bool,
    pub closed: bool,
    pub bump: u8,
}

#[derive(Accounts)]
#[instruction(vault_id: u64)]
pub struct Open<'info> {
    #[account(mut)]
    pub user: Signer<'info>,
    #[account(init, payer = user, space = 8 + Vault::INIT_SPACE,
              seeds = [b"vault", user.key().as_ref(), &vault_id.to_le_bytes()], bump)]
    pub vault: Account<'info, Vault>,
    pub system_program: Program<'info, System>,
}

#[derive(Accounts)]
pub struct Settle<'info> {
    #[account(mut, has_one = user, has_one = exchange)]
    pub vault: Account<'info, Vault>,
    pub user: Signer<'info>,
    pub exchange: Signer<'info>,
    /// CHECK: checked against vault.user in pay_out
    #[account(mut)]
    pub user_wallet: AccountInfo<'info>,
    /// CHECK: checked against vault.exchange in pay_out
    #[account(mut)]
    pub exchange_wallet: AccountInfo<'info>,
}

#[derive(Accounts)]
pub struct RequestExit<'info> {
    #[account(mut)]
    pub vault: Account<'info, Vault>,
    pub caller: Signer<'info>,
}

#[derive(Accounts)]
pub struct SubmitState<'info> {
    #[account(mut)]
    pub vault: Account<'info, Vault>,
    pub submitter: Signer<'info>,
    /// CHECK: the Instructions sysvar, address-checked
    #[account(address = ix_sysvar::ID)]
    pub instructions: AccountInfo<'info>,
}

#[derive(Accounts)]
pub struct Finalize<'info> {
    #[account(mut)]
    pub vault: Account<'info, Vault>,
    pub caller: Signer<'info>,
    /// CHECK: checked against vault.user in pay_out
    #[account(mut)]
    pub user_wallet: AccountInfo<'info>,
    /// CHECK: checked against vault.exchange in pay_out
    #[account(mut)]
    pub exchange_wallet: AccountInfo<'info>,
}

#[error_code]
pub enum CastleError {
    #[msg("Nothing locked")]
    NothingLocked,
    #[msg("Exit delay too short")]
    DelayTooShort,
    #[msg("Bad exchange address")]
    BadExchange,
    #[msg("Vault is closed")]
    Closed,
    #[msg("An exit is in progress; submit a state instead")]
    ExitInProgress,
    #[msg("Split is larger than the vault")]
    SplitTooLarge,
    #[msg("Only the user or exchange can do this")]
    NotAParty,
    #[msg("An exit was already requested")]
    ExitAlreadyRequested,
    #[msg("No exit has been requested")]
    NoExit,
    #[msg("The exit window has ended")]
    WindowEnded,
    #[msg("The exit window is still running")]
    WindowRunning,
    #[msg("A newer state was already submitted")]
    NewerStateExists,
    #[msg("Missing a valid user signature")]
    MissingUserSignature,
    #[msg("Missing a valid exchange signature")]
    MissingExchangeSignature,
    #[msg("Payout wallet doesn't match the vault")]
    WrongWallet,
}
