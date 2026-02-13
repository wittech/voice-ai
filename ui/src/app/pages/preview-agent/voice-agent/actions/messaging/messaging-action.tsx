import { FC, HTMLAttributes, useCallback, useState } from 'react';
import {
  Assistant,
  Channel,
  useConnectAgent,
  useInputModeToggleAgent,
  VoiceAgent,
} from '@rapidaai/react';
import { cn } from '@/utils';
import { MicOff, Phone, PhoneOff } from 'lucide-react';
import { Spinner } from '@/app/components/loader/spinner';
import { AudioMessagingAction } from './audio-messsaging-action';
import { SimpleMessagingAction } from './simple-messaging-action';
import { ICancelButton, IRedBGButton } from '@/app/components/form/button';

/**
 *
 */
interface MessageActionProps extends HTMLAttributes<HTMLDivElement> {
  voiceAgent: VoiceAgent;
  assistant: Assistant | null;
  suggestions?: string[];
  placeholder?: string;
}

/**
 *
 * @param param0
 * @returns
 */
export const MessagingAction: FC<MessageActionProps> = ({
  suggestions = [],
  className,
  ...attr
}) => {
  const { channel } = useInputModeToggleAgent(attr.voiceAgent);
  return (
    <div className={cn(className)}>
      {channel === Channel.Audio ? (
        <AudioMessagingAction {...attr} />
      ) : (
        <SimpleMessagingAction {...attr} />
      )}
    </div>
  );
};
