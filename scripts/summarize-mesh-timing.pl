#!/usr/bin/env perl
use strict;
use warnings;

use File::Find qw(find);
use JSON::PP qw(decode_json);
use Time::Piece;

sub collect_log_paths {
  my @inputs = @_;
  my %seen = ();
  my @paths = ();

  for my $input (@inputs) {
    next unless defined $input && $input ne '';
    if (-d $input) {
      find(
        sub {
          return unless -f $_;
          return unless $_ =~ /\.log$/;
          return if $seen{$File::Find::name}++;
          push @paths, $File::Find::name;
        },
        $input,
      );
      next;
    }
    next unless -f $input;
    next if $seen{$input}++;
    push @paths, $input;
  }

  @paths = sort @paths;
  return @paths;
}

sub parse_finished_at_ms {
  my ($value) = @_;
  return undef unless defined $value;
  return undef unless $value =~ /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(\.(\d+))?Z$/;

  my $base = eval { Time::Piece->strptime($1, '%Y-%m-%dT%H:%M:%S') };
  return undef unless defined $base;

  my $millis = int($base->epoch * 1000);
  my $fraction = defined $3 ? $3 : '';
  if ($fraction ne '') {
    $fraction .= '000';
    $millis += int(substr($fraction, 0, 3));
  }
  return $millis;
}

sub percentile {
  my ($values, $fraction) = @_;
  return 0 unless @$values;
  my @sorted = sort { $a <=> $b } @$values;
  my $index = int(($#sorted) * $fraction + 0.5);
  $index = 0 if $index < 0;
  $index = $#sorted if $index > $#sorted;
  return $sorted[$index];
}

sub average {
  my ($values) = @_;
  return 0 unless @$values;
  my $total = 0;
  $total += $_ for @$values;
  return $total / scalar(@$values);
}

sub fmt_ms {
  my ($value) = @_;
  return sprintf('%.1fms', $value // 0);
}

sub metric_label {
  my ($entry) = @_;
  my $label = $entry->{metric} // '';
  if (defined $entry->{purpose} && $entry->{purpose} ne '') {
    $label .= " (purpose=$entry->{purpose})";
  }
  return $label;
}

my @paths = collect_log_paths(@ARGV);
if (!@paths) {
  print "No log files found.\n";
  exit 0;
}

my @entries = ();
my $order = 0;

for my $path (@paths) {
  open my $fh, '<', $path or next;
  while (my $line = <$fh>) {
    next unless $line =~ /\[mesh-timing\]/;
    next unless $line =~ /\[mesh-timing\]\s+(\{.*\})\s*$/;
    my $raw = $1;
    my $entry = eval { decode_json($raw) };
    next unless $entry && ref($entry) eq 'HASH';
    $order += 1;
    $entry->{_source} = $path;
    $entry->{_order} = $order;
    $entry->{_finished_ms} = parse_finished_at_ms($entry->{finishedAt});
    push @entries, $entry;
  }
  close $fh;
}

if (!@entries) {
  print "No mesh timing entries found.\n";
  exit 0;
}

print "Timing summary from ", scalar(@entries), " entries across ", scalar(@paths), " log files\n";
print "\nMetric stats\n";

my %metric_groups = ();
for my $entry (@entries) {
  my $label = metric_label($entry);
  push @{ $metric_groups{$label} }, $entry->{durationMs} + 0;
}

for my $label (sort keys %metric_groups) {
  my $values = $metric_groups{$label};
  my @sorted = sort { $a <=> $b } @$values;
  printf "  %-42s count=%-4d avg=%-8s p50=%-8s p95=%-8s max=%-8s\n",
    $label,
    scalar(@sorted),
    fmt_ms(average(\@sorted)),
    fmt_ms(percentile(\@sorted, 0.50)),
    fmt_ms(percentile(\@sorted, 0.95)),
    fmt_ms($sorted[-1]);
}

print "\nPer-custodySeq action timelines\n";
my %timeline_groups = ();
my %action_roots = ();
for my $entry (@entries) {
  next unless defined $entry->{custodySeq} && $entry->{custodySeq} ne '' && $entry->{custodySeq} > 0;
  next unless defined $entry->{tableId} && $entry->{tableId} ne '';
  my $key = join("\t", $entry->{tableId}, $entry->{custodySeq});
  push @{ $timeline_groups{$key} }, $entry;
  next unless ($entry->{metric} // '') eq 'action_transition_total';
  next unless defined $entry->{_finished_ms};
  my $started_ms = $entry->{_finished_ms} - ($entry->{durationMs} + 0);
  if (!defined $action_roots{$key} || $started_ms < $action_roots{$key}) {
    $action_roots{$key} = $started_ms;
  }
}

my @timeline_keys = sort {
  my ($left_table, $left_seq) = split(/\t/, $a, 2);
  my ($right_table, $right_seq) = split(/\t/, $b, 2);
  $left_table cmp $right_table || $left_seq <=> $right_seq;
} grep { defined $action_roots{$_} } keys %timeline_groups;

if (!@timeline_keys) {
  print "  No action-transition timelines found.\n";
} else {
  for my $key (@timeline_keys) {
    my ($table_id, $seq) = split(/\t/, $key, 2);
    my @group = sort {
      (($a->{_finished_ms} // 0) <=> ($b->{_finished_ms} // 0)) ||
      (($a->{_order} // 0) <=> ($b->{_order} // 0))
    } @{ $timeline_groups{$key} };

    my $action_entry = undef;
    for my $entry (@group) {
      next unless ($entry->{metric} // '') eq 'action_transition_total';
      $action_entry = $entry;
      last;
    }

    my $root_ms = $action_roots{$key};
    my $total = defined $action_entry ? $action_entry->{durationMs} + 0 : 0;
    print "  table=$table_id seq=$seq total=", fmt_ms($total), "\n";
    for my $entry (@group) {
      my $offset = defined $entry->{_finished_ms} ? ($entry->{_finished_ms} - $root_ms) : 0;
      my $label = metric_label($entry);
      if (defined $entry->{bundleKind} && $entry->{bundleKind} ne '') {
        $label .= " bundle=$entry->{bundleKind}";
      }
      printf "    +%-8s %-44s duration=%-8s ok=%s\n",
        fmt_ms($offset),
        $label,
        fmt_ms($entry->{durationMs} + 0),
        (($entry->{ok}) ? 'true' : 'false');
    }
  }
}

print "\nRecovery\n";
my @recovery_attach = grep { ($_->{metric} // '') eq 'recovery_bundle_attach' } @entries;
my @recovery_sign = grep { ($_->{metric} // '') eq 'custody_psbt_fully_sign' && ($_->{purpose} // '') eq 'recovery' } @entries;
my @recovery_finalize = grep { ($_->{metric} // '') eq 'recovery_finalize' } @entries;

my $total_bundles = 0;
$total_bundles += ($_->{bundleCount} // 0) for @recovery_attach;
printf "  bundle attachments: count=%d totalBundles=%d avgBundles=%s\n",
  scalar(@recovery_attach),
  $total_bundles,
  scalar(@recovery_attach) ? sprintf('%.2f', $total_bundles / scalar(@recovery_attach)) : '0.00';

if (@recovery_sign) {
  my @durations = map { $_->{durationMs} + 0 } @recovery_sign;
  printf "  recovery signing:  count=%d avg=%s p50=%s p95=%s max=%s\n",
    scalar(@recovery_sign),
    fmt_ms(average(\@durations)),
    fmt_ms(percentile(\@durations, 0.50)),
    fmt_ms(percentile(\@durations, 0.95)),
    fmt_ms((sort { $a <=> $b } @durations)[-1]);
} else {
  print "  recovery signing:  no entries\n";
}

if (@recovery_finalize) {
  my @durations = map { $_->{durationMs} + 0 } @recovery_finalize;
  printf "  recovery finalize: count=%d avg=%s p50=%s p95=%s max=%s\n",
    scalar(@recovery_finalize),
    fmt_ms(average(\@durations)),
    fmt_ms(percentile(\@durations, 0.50)),
    fmt_ms(percentile(\@durations, 0.95)),
    fmt_ms((sort { $a <=> $b } @durations)[-1]);
} else {
  print "  recovery finalize: no entries\n";
}
