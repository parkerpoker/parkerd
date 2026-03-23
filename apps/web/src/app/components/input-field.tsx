import type { InputHTMLAttributes } from 'react';

interface InputFieldProps extends InputHTMLAttributes<HTMLInputElement> {
  label?: string;
}

export function InputField({ label, className = '', ...props }: InputFieldProps) {
  return (
    <div className="space-y-1.5">
      {label && (
        <label className="text-xs text-muted-foreground uppercase tracking-wide">
          {label}
        </label>
      )}
      <input
        className={`w-full rounded-md border border-border bg-input px-3 py-2 text-sm text-foreground placeholder:text-muted-foreground focus:border-primary focus:outline-none focus:ring-1 focus:ring-primary transition-colors ${className}`}
        {...props}
      />
    </div>
  );
}
