import * as React from 'react'
import { ChevronDown } from 'lucide-react'
import { cn } from '../../lib/utils'

export interface SelectProps extends React.SelectHTMLAttributes<HTMLSelectElement> {
  placeholder?: string
}

// Keep native select semantics instead of reimplementing a partial combobox.
// This preserves controlled and uncontrolled/defaultValue behavior, form
// integration, disabled options, tabIndex, and the platform's complete keyboard
// and assistive-technology interaction model.
const Select = React.forwardRef<HTMLSelectElement, SelectProps>(
  ({ className, children, placeholder = 'Select...', ...props }, ref) => {
    const hasEmptyOption = React.Children.toArray(children).some((child) => (
      React.isValidElement<{ value?: string | number }>(child)
      && child.type === 'option'
      && String(child.props.value ?? '') === ''
    ))

    return (
      <div className="relative w-full">
        <select
          ref={ref}
          className={cn(
            'flex h-9 w-full appearance-none rounded-md border border-input bg-background px-3 py-2 pr-8 text-sm shadow-sm ring-offset-background focus:outline-none focus:ring-1 focus:ring-ring disabled:cursor-not-allowed disabled:opacity-50',
            className,
          )}
          {...props}
        >
          {!hasEmptyOption && placeholder ? (
            <option value="" disabled hidden>{placeholder}</option>
          ) : null}
          {children}
        </select>
        <ChevronDown
          className="pointer-events-none absolute right-3 top-1/2 h-4 w-4 -translate-y-1/2 opacity-50"
          aria-hidden="true"
        />
      </div>
    )
  },
)
Select.displayName = 'Select'

export { Select }
