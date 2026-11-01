import { HoverCard as HoverCardPrimitive } from "radix-ui";
import { useCallback, useEffect, useRef } from "react";
import { cn } from "../../lib/utils";

function HoverCard({
	openDelay = 300,
	closeDelay = 150,
	open,
	onOpenChange,
	...props
}: React.ComponentProps<typeof HoverCardPrimitive.Root>) {
	// A card anchored to a row that scrolls away (chat auto-scroll, streaming
	// timeline updates, board re-layout) must not keep floating over unrelated
	// content: Radix hover cards have no scroll dismissal, so the open card
	// blocks whatever ends up underneath it and its pointer churn burns CPU
	// until an unrelated click dismisses it. Dismiss as soon as anything
	// scrolls while a card is open — except scrolls inside the card itself
	// (the link-preview card scrolls its own overflow content).
	const isOpenRef = useRef(false);
	const handleOpenChange = useCallback(
		(next: boolean) => {
			isOpenRef.current = next;
			onOpenChange?.(next);
		},
		[onOpenChange],
	);
	useEffect(() => {
		if (open !== undefined) isOpenRef.current = open;
	}, [open]);
	useEffect(() => {
		const onScroll = (event: Event) => {
			if (!isOpenRef.current) return;
			if (event.target instanceof Element && event.target.closest('[data-slot="hover-card-content"]')) return;
			handleOpenChange(false);
		};
		window.addEventListener("scroll", onScroll, true);
		return () => window.removeEventListener("scroll", onScroll, true);
	}, [handleOpenChange]);
	return (
		<HoverCardPrimitive.Root
			data-slot="hover-card"
			openDelay={openDelay}
			closeDelay={closeDelay}
			open={open}
			onOpenChange={handleOpenChange}
			{...props}
		/>
	);
}

function HoverCardTrigger({ ...props }: React.ComponentProps<typeof HoverCardPrimitive.Trigger>) {
	return <HoverCardPrimitive.Trigger data-slot="hover-card-trigger" {...props} />;
}

function HoverCardContent({
	className,
	align = "start",
	sideOffset = 6,
	portalContainer,
	...props
}: React.ComponentProps<typeof HoverCardPrimitive.Content> & {
	portalContainer?: React.ComponentProps<typeof HoverCardPrimitive.Portal>["container"];
}) {
	return (
		<HoverCardPrimitive.Portal container={portalContainer}>
			<HoverCardPrimitive.Content
				data-slot="hover-card-content"
				align={align}
				sideOffset={sideOffset}
				className={cn(
					"z-overlay w-72 origin-(--radix-hover-card-content-transform-origin) animate-in rounded-lg border border-border bg-popover text-popover-foreground shadow-md outline-none fade-in-0 zoom-in-95 data-[side=bottom]:slide-in-from-top-2 data-[side=left]:slide-in-from-right-2 data-[side=right]:slide-in-from-left-2 data-[side=top]:slide-in-from-bottom-2 data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=closed]:zoom-out-95",
					className,
				)}
				{...props}
			/>
		</HoverCardPrimitive.Portal>
	);
}

export { HoverCard, HoverCardTrigger, HoverCardContent };
