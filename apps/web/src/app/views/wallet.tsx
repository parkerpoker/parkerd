import type { LocalProfileStatusResponse } from '../../lib/localControllerApi.js';

import { Button } from '../components/button.js';
import { InputField } from '../components/input-field.js';
import { Panel } from '../components/panel.js';

interface WalletViewProps {
  profileStatus: LocalProfileStatusResponse | null;
  selectedProfile: string | null;
  busyAction: string | null;
  networkName: string;
  depositAmount: string;
  onDepositAmountChange: (value: string) => void;
  withdrawAmount: string;
  onWithdrawAmountChange: (value: string) => void;
  withdrawInvoice: string;
  onWithdrawInvoiceChange: (value: string) => void;
  faucetAmount: string;
  onFaucetAmountChange: (value: string) => void;
  offboardAddress: string;
  onOffboardAddressChange: (value: string) => void;
  offboardAmount: string;
  onOffboardAmountChange: (value: string) => void;
  onDeposit: () => void;
  onWithdraw: () => void;
  onFaucet: () => void;
  onOnboard: () => void;
  onOffboard: () => void;
}

function formatSats(value?: number | null) {
  return `${(value ?? 0).toLocaleString()} sat`;
}

export function WalletView({
  profileStatus,
  selectedProfile,
  busyAction,
  networkName,
  depositAmount,
  onDepositAmountChange,
  withdrawAmount,
  onWithdrawAmountChange,
  withdrawInvoice,
  onWithdrawInvoiceChange,
  faucetAmount,
  onFaucetAmountChange,
  offboardAddress,
  onOffboardAddressChange,
  offboardAmount,
  onOffboardAmountChange,
  onDeposit,
  onWithdraw,
  onFaucet,
  onOnboard,
  onOffboard,
}: WalletViewProps) {
  const localWallet = profileStatus?.daemon.state?.wallet;
  const disabled = !selectedProfile || busyAction !== null;

  return (
    <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
      {/* Balances */}
      <Panel title="Wallet / Bankroll">
        <div className="space-y-4">
          <div className="grid grid-cols-2 gap-3">
            <div className="rounded-md border border-border bg-secondary/50 p-3">
              <div className="text-xs text-muted-foreground uppercase tracking-wide">Available</div>
              <div className="mt-1 text-lg text-primary">{formatSats(localWallet?.availableSats)}</div>
            </div>
            <div className="rounded-md border border-border bg-secondary/50 p-3">
              <div className="text-xs text-muted-foreground uppercase tracking-wide">Total</div>
              <div className="mt-1 text-lg text-foreground">{formatSats(localWallet?.totalSats)}</div>
            </div>
          </div>

          <div className="flex gap-2 text-xs">
            <div className="flex-1 rounded border border-border bg-input px-2 py-1.5">
              <span className="text-muted-foreground">Ark Receive: </span>
              <span className="text-[#2ecc71] truncate">{localWallet?.arkAddress ? 'Ready' : 'n/a'}</span>
            </div>
            <div className="flex-1 rounded border border-border bg-input px-2 py-1.5">
              <span className="text-muted-foreground">Boarding: </span>
              <span className="text-[#2ecc71] truncate">{localWallet?.boardingAddress ? 'Active' : 'n/a'}</span>
            </div>
          </div>

          {localWallet?.arkAddress && (
            <div className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
              <div className="text-muted-foreground">Ark Address</div>
              <div className="text-foreground break-all font-mono">{localWallet.arkAddress}</div>
            </div>
          )}
          {localWallet?.boardingAddress && (
            <div className="rounded-md border border-border bg-secondary/50 p-2 text-xs">
              <div className="text-muted-foreground">Boarding Address</div>
              <div className="text-foreground break-all font-mono">{localWallet.boardingAddress}</div>
            </div>
          )}
        </div>
      </Panel>

      {/* Deposit & Withdraw */}
      <Panel title="Deposit / Withdraw">
        <div className="space-y-6">
          <div className="space-y-2">
            <InputField label="Deposit Amount (sats)" type="number" value={depositAmount} onChange={(e) => onDepositAmountChange(e.target.value)} placeholder="Amount (sats)" />
            <Button variant="primary" onClick={onDeposit} disabled={disabled} className="w-full">
              Create Deposit Quote
            </Button>
          </div>

          <div className="space-y-2">
            <InputField label="Withdraw Amount (sats)" type="number" value={withdrawAmount} onChange={(e) => onWithdrawAmountChange(e.target.value)} placeholder="Amount (sats)" />
            <InputField label="Lightning Invoice" value={withdrawInvoice} onChange={(e) => onWithdrawInvoiceChange(e.target.value)} placeholder="Lightning invoice" />
            <Button variant="primary" onClick={onWithdraw} disabled={disabled} className="w-full">
              Send Withdrawal
            </Button>
          </div>

          {networkName === 'regtest' && (
            <div className="space-y-2">
              <InputField label="Faucet Amount (sats)" type="number" value={faucetAmount} onChange={(e) => onFaucetAmountChange(e.target.value)} placeholder="Amount (sats)" />
              <Button variant="secondary" onClick={onFaucet} disabled={disabled} className="w-full">
                Request Faucet
              </Button>
            </div>
          )}
        </div>
      </Panel>

      {/* On-chain Bridging */}
      <Panel title="On-chain Bridging">
        <div className="space-y-4">
          <InputField label="BTC Address" value={offboardAddress} onChange={(e) => onOffboardAddressChange(e.target.value)} placeholder="BTC address" />
          <InputField label="Amount (optional)" type="number" value={offboardAmount} onChange={(e) => onOffboardAmountChange(e.target.value)} placeholder="Amount (optional sats)" />
          <div className="flex gap-2">
            <Button variant="secondary" onClick={onOnboard} disabled={disabled} className="flex-1">
              Onboard
            </Button>
            <Button variant="secondary" onClick={onOffboard} disabled={disabled} className="flex-1">
              Offboard
            </Button>
          </div>
        </div>
      </Panel>
    </div>
  );
}
