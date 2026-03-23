import type { ButtonHTMLAttributes, ReactNode } from 'react';

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: 'primary' | 'secondary' | 'danger';
  children: ReactNode;
}

export function Button({ variant = 'secondary', children, className = '', ...props }: ButtonProps) {
  const variants = {
    primary: 'bg-primary text-primary-foreground hover:bg-primary/90 border-primary',
    secondary: 'bg-secondary text-secondary-foreground hover:bg-secondary/80 border-border',
    danger: 'bg-destructive/20 text-destructive hover:bg-destructive/30 border-destructive/30',
  };

  return (
    <button
      className={`rounded-md border px-4 py-2 text-sm transition-colors focus:outline-none focus:ring-2 focus:ring-primary focus:ring-offset-2 focus:ring-offset-background disabled:opacity-50 disabled:cursor-not-allowed ${variants[variant]} ${className}`}
      {...props}
    >
      {children}
    </button>
  );
}
